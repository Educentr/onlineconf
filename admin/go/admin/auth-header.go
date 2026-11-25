package admin

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/rs/zerolog/log"

	. "github.com/onlineconf/onlineconf/admin/go/common"
)

// maxGroupNameLength is the capacity of my_config_group.Name (varchar(128)).
const maxGroupNameLength = 128

// HeaderAuthenticatorConfig configures the authentication performed by a
// reverse proxy deployed in front of onlineconf-admin.
type HeaderAuthenticatorConfig struct {
	// Header is the name of the HTTP header the proxy passes the username in,
	// X-Username by default.
	Header string
	// Secret is the salt of the signature of the headers, see signData. An
	// empty Secret turns the verification of the signature off.
	Secret string
	// GroupsHeader is the name of the HTTP header the proxy passes the groups
	// of the user in, separated by commas (X-Forwarded-Groups of oauth2-proxy,
	// for example). An empty GroupsHeader turns the synchronization of the
	// groups off: the membership is managed in the UI only, as before.
	GroupsHeader string `yaml:"groups_header"`
	// GroupsPrefix is the prefix of the groups of GroupsHeader taken into
	// account. The groups without the prefix are ignored, the prefix is cut off
	// the rest (/onlineconf/back-developer becomes back-developer with the
	// prefix /onlineconf/). An empty GroupsPrefix takes every group as is.
	GroupsPrefix string `yaml:"groups_prefix"`
}

type headerAuthenticator struct {
	config HeaderAuthenticatorConfig
	groups userGroupStore
}

// userGroupStore is the part of the storage of the groups the synchronization
// of the membership with the groups header needs. It is an interface to make
// the synchronization testable without a database.
type userGroupStore interface {
	SelectUserGroups(ctx context.Context, user string) ([]string, error)
	CreateGroupIfNotExists(ctx context.Context, name string) (bool, error)
	AddUserToGroupIfNotExists(ctx context.Context, group, user string) (bool, error)
	DeleteUserFromGroup(ctx context.Context, group, user string) error
}

func NewHeaderAuthenticator(config HeaderAuthenticatorConfig) *headerAuthenticator {
	if config.Header == "" {
		config.Header = "X-Username"
	}
	if config.GroupsHeader == "" && config.GroupsPrefix != "" {
		log.Warn().Msg("auth.groups_prefix is ignored because auth.groups_header is not configured")
	}
	if config.GroupsHeader != "" {
		log.Info().
			Str("groups_header", config.GroupsHeader).
			Str("groups_prefix", config.GroupsPrefix).
			Msg("groups of a user are synchronized with the groups header")
	}
	return &headerAuthenticator{
		config: config,
		groups: dbUserGroupStore{},
	}
}

func (auth *headerAuthenticator) Authenticate(req *http.Request) (string, error) {
	username := req.Header.Get(auth.config.Header)
	groups, groupsSent := auth.groupsFromRequest(req)

	if auth.config.Secret != "" && !auth.checkSign(req, username, groups, groupsSent) {
		return "", nil
	}
	if username == "" {
		return username, nil
	}

	// A request without the groups header does not change the membership: this
	// way the emergency login by a password and the clients having no such
	// header keep working.
	if !groupsSent {
		return username, nil
	}
	ctx := req.Context()
	if err := auth.syncUserGroups(ctx, username, auth.parseGroups(ctx, groups)); err != nil {
		return "", fmt.Errorf("synchronize groups of user %s: %w", username, err)
	}
	return username, nil
}

// groupsFromRequest returns the raw value of the groups header and reports
// whether the request carries it at all. Only the first occurrence of the
// header is honoured.
func (auth *headerAuthenticator) groupsFromRequest(req *http.Request) (string, bool) {
	if auth.config.GroupsHeader == "" {
		return "", false
	}
	values := req.Header.Values(auth.config.GroupsHeader)
	if len(values) == 0 {
		return "", false
	}
	if len(values) > 1 {
		log.Ctx(req.Context()).Warn().
			Str("header", auth.config.GroupsHeader).
			Int("count", len(values)).
			Msg("the request carries several groups headers, only the first one is used")
	}
	return values[0], true
}

// checkSign verifies the signature passed in the "<header>-Sign" header.
func (auth *headerAuthenticator) checkSign(req *http.Request, username, groups string, groupsSent bool) bool {
	signHeader := req.Header.Get(auth.config.Header + "-Sign")
	if signHeader == "" {
		return false
	}
	sign, err := hex.DecodeString(signHeader)
	if err != nil {
		return false
	}

	remoteIP, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		remoteIP = req.RemoteAddr
	}

	h := md5.New()
	h.Write(auth.signData(username, groups, groupsSent, remoteIP))
	return bytes.Equal(sign, h.Sum(nil))
}

// signData returns the exact byte sequence the signature of the headers is
// calculated over.
//
// Without groups_header the sequence is the historical bare concatenation, so
// that the installations configured with a secret keep working unchanged:
//
//	<username><remote ip><secret>
//
// With groups_header the groups take part in the signature as well, otherwise
// they could be forged. The fields are separated by "\n", a byte that cannot
// occur in a value of an HTTP header:
//
//	<username>\n<groups field>\n<remote ip><secret>
//
// where <groups field> is the raw value of the groups header prefixed with "+",
// or just "-" if the request carries no groups header. The marker keeps the two
// cases distinct: without it a request carrying an empty groups header ("drop
// every group of the user") and a request carrying no groups header at all
// ("keep the membership untouched") would have the same signature, and one
// could be replaced by the other.
func (auth *headerAuthenticator) signData(username, groups string, groupsSent bool, remoteIP string) []byte {
	var buf bytes.Buffer
	buf.WriteString(username)
	if auth.config.GroupsHeader != "" {
		buf.WriteString("\n")
		if groupsSent {
			buf.WriteString("+")
			buf.WriteString(groups)
		} else {
			buf.WriteString("-")
		}
		buf.WriteString("\n")
	}
	buf.WriteString(remoteIP)
	buf.WriteString(auth.config.Secret)
	return buf.Bytes()
}

// parseGroups converts the raw value of the groups header into a list of names
// of OnlineConf groups: it splits the value by commas, trims the spaces around
// every group, keeps the groups matching groups_prefix only and cuts the prefix
// off.
//
// A group OnlineConf cannot represent is skipped with a warning instead of
// failing the whole request: a nested Keycloak group (its name still contains
// "/" after the prefix is cut off, and "/" breaks the /group/{group} routes of
// the API and the UI) and a name longer than my_config_group.Name.
func (auth *headerAuthenticator) parseGroups(ctx context.Context, raw string) []string {
	groups := make([]string, 0)
	seen := make(map[string]bool)
	for _, field := range strings.Split(raw, ",") {
		name := strings.TrimSpace(field)
		if auth.config.GroupsPrefix != "" {
			if !strings.HasPrefix(name, auth.config.GroupsPrefix) {
				continue
			}
			name = name[len(auth.config.GroupsPrefix):]
		}
		if name == "" {
			continue
		}
		if strings.Contains(name, "/") {
			log.Ctx(ctx).Warn().Str("group", name).Msg("a group of the groups header is skipped: a nested group is not supported")
			continue
		}
		if utf8.RuneCountInString(name) > maxGroupNameLength {
			log.Ctx(ctx).Warn().Str("group", name).Msg("a group of the groups header is skipped: the name is too long")
			continue
		}
		if seen[foldGroupName(name)] {
			continue
		}
		seen[foldGroupName(name)] = true
		groups = append(groups, name)
	}
	return groups
}

// syncUserGroups brings the groups of the user stored in the database in line
// with the groups listed in the groups header: the user is added to the groups
// it is missing from and is removed from the groups the header does not list.
// The groups absent from the database are created without any rights on the
// tree, the rights are still granted in the UI.
//
// The membership is written to my_config_user_group and not just kept in the
// request, because the access to the tree is checked by the SQL functions
// (my_config_tree_access and others) reading that table. The database is
// written only when the membership differs from the header, not on every
// request.
func (auth *headerAuthenticator) syncUserGroups(ctx context.Context, user string, groups []string) error {
	current, err := auth.groups.SelectUserGroups(ctx, user)
	if err != nil {
		return fmt.Errorf("select groups of the user: %w", err)
	}

	currentSet := make(map[string]bool, len(current))
	for _, group := range current {
		currentSet[foldGroupName(group)] = true
	}
	wanted := make(map[string]bool, len(groups))
	for _, group := range groups {
		wanted[foldGroupName(group)] = true
	}

	for _, group := range groups {
		if currentSet[foldGroupName(group)] {
			continue
		}
		created, err := auth.groups.CreateGroupIfNotExists(ctx, group)
		if err != nil {
			return fmt.Errorf("create group %s: %w", group, err)
		}
		if created {
			log.Ctx(ctx).Info().Str("group", group).Msg("a group of the groups header is created without rights")
		}
		added, err := auth.groups.AddUserToGroupIfNotExists(ctx, group, user)
		if err != nil {
			return fmt.Errorf("add the user to group %s: %w", group, err)
		}
		if added {
			log.Ctx(ctx).Info().Str("group", group).Str("user", user).Msg("the user is added to a group by the groups header")
		}
	}

	for _, group := range current {
		if wanted[foldGroupName(group)] {
			continue
		}
		if err := auth.groups.DeleteUserFromGroup(ctx, group, user); err != nil {
			return fmt.Errorf("delete the user from group %s: %w", group, err)
		}
		log.Ctx(ctx).Info().Str("group", group).Str("user", user).Msg("the user is removed from a group by the groups header")
	}

	return nil
}

// foldGroupName returns the key the names of the groups are compared by.
//
// The names are compared case-insensitively, because the unique key of
// my_config_group.Name is case-insensitive as well: the group Root cannot be
// created next to the group root, and both the API and the UI address the
// existing one by either name. Comparing the names exactly would make every
// request of a user whose Keycloak group differs from the group of the database
// in case only remove it from the group and add it back.
func foldGroupName(name string) string {
	return strings.ToLower(name)
}

// dbUserGroupStore is the userGroupStore backed by the database of OnlineConf.
type dbUserGroupStore struct{}

func (dbUserGroupStore) SelectUserGroups(ctx context.Context, user string) ([]string, error) {
	return SelectUserGroups(ctx, user)
}

func (dbUserGroupStore) CreateGroupIfNotExists(ctx context.Context, name string) (bool, error) {
	return CreateGroupIfNotExists(ctx, name)
}

func (dbUserGroupStore) AddUserToGroupIfNotExists(ctx context.Context, group, user string) (bool, error) {
	return AddUserToGroupIfNotExists(ctx, group, user)
}

func (dbUserGroupStore) DeleteUserFromGroup(ctx context.Context, group, user string) error {
	return DeleteUserFromGroup(ctx, group, user)
}

func (auth *headerAuthenticator) SelectUsers(ctx context.Context, term string) ([]string, error) {
	order := "1"
	bind := make([]interface{}, 0)
	query := "SELECT DISTINCT User FROM my_config_user_group"
	if term != "" {
		query += " WHERE User LIKE ?"
		bind = append(bind, "%"+termRe.ReplaceAllString(term, "\\$1")+"%", term)
		order = "INSTR(User, ?), 1"
	}
	query += " ORDER BY " + order
	rows, err := DB.QueryContext(ctx, query, bind...)
	if err != nil {
		return nil, err
	}
	return ReadStrings(rows)
}
