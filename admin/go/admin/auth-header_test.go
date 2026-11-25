package admin

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// collationFolder imitates the part of the collation of my_config_group.Name
// the tests rely on: utf8mb4_0900_ai_ci ignores the accents, so café, cafe and
// CAFE are one and the same name. Only the equivalences the tests use are
// listed, the folding of the case is left to strings.ToLower.
var collationFolder = strings.NewReplacer("é", "e", "ё", "е", "ß", "ss")

// foldLikeCollation returns the key the "database" compares the names of the
// groups by. Nothing of the kind exists in the code under test on purpose: the
// comparison of the names belongs to the database, see Group.
func foldLikeCollation(name string) string {
	return collationFolder.Replace(strings.ToLower(name))
}

// fakeUserGroupStore is an in-memory userGroupStore recording every write.
//
// The names of the groups are compared the way the database compares them: the
// store holds a single group of the names that differ in the case or in the
// accents only and resolves every one of those names to it, as the unique key
// of my_config_group.Name does.
type fakeUserGroupStore struct {
	groups     []Group          // the groups existing in the "database"
	membership map[string][]int // user -> the IDs of the groups of the user
	writes     []string         // the writes performed, in order
	selectErr  error
}

func newFakeUserGroupStore(groups []string, membership map[string][]string) *fakeUserGroupStore {
	store := &fakeUserGroupStore{membership: map[string][]int{}}
	for _, name := range groups {
		store.createGroup(name)
	}
	for user, names := range membership {
		for _, name := range names {
			group, ok := store.findGroup(name)
			if !ok {
				panic("the fixture puts " + user + " into the group " + name + " which does not exist")
			}
			store.membership[user] = append(store.membership[user], group.ID)
		}
	}
	return store
}

func (s *fakeUserGroupStore) createGroup(name string) Group {
	group := Group{ID: len(s.groups) + 1, Name: name}
	s.groups = append(s.groups, group)
	return group
}

// findGroup returns the group stored under the name, the names being compared
// the way the database compares them.
func (s *fakeUserGroupStore) findGroup(name string) (Group, bool) {
	for _, group := range s.groups {
		if foldLikeCollation(group.Name) == foldLikeCollation(name) {
			return group, true
		}
	}
	return Group{}, false
}

func (s *fakeUserGroupStore) findGroupID(id int) (Group, bool) {
	for _, group := range s.groups {
		if group.ID == id {
			return group, true
		}
	}
	return Group{}, false
}

// groupNames returns the names of the groups existing in the "database".
func (s *fakeUserGroupStore) groupNames() []string {
	names := make([]string, 0, len(s.groups))
	for _, group := range s.groups {
		names = append(names, group.Name)
	}
	return names
}

// userGroups returns the names of the groups of the user, sorted.
func (s *fakeUserGroupStore) userGroups(user string) []string {
	names := make([]string, 0, len(s.membership[user]))
	for _, id := range s.membership[user] {
		group, ok := s.findGroupID(id)
		if !ok {
			panic("the user is a member of a group that does not exist")
		}
		names = append(names, group.Name)
	}
	sort.Strings(names)
	return names
}

func (s *fakeUserGroupStore) SelectUserGroups(ctx context.Context, user string) ([]Group, error) {
	if s.selectErr != nil {
		return nil, s.selectErr
	}
	groups := make([]Group, 0, len(s.membership[user]))
	for _, name := range s.userGroups(user) {
		group, _ := s.findGroup(name)
		groups = append(groups, group)
	}
	return groups, nil
}

func (s *fakeUserGroupStore) SelectGroupsByNames(ctx context.Context, names []string) ([]*Group, error) {
	if s.selectErr != nil {
		return nil, s.selectErr
	}
	resolved := make([]*Group, len(names))
	for i, name := range names {
		if group, ok := s.findGroup(name); ok {
			found := group
			resolved[i] = &found
		}
	}
	return resolved, nil
}

func (s *fakeUserGroupStore) CreateGroupIfNotExists(ctx context.Context, name string) (bool, error) {
	if _, ok := s.findGroup(name); ok {
		return false, nil
	}
	s.createGroup(name)
	s.writes = append(s.writes, "create group "+name)
	return true, nil
}

func (s *fakeUserGroupStore) AddUserToGroupIDIfNotExists(ctx context.Context, groupID int, user string) (bool, error) {
	group, ok := s.findGroupID(groupID)
	if !ok {
		return false, nil
	}
	for _, id := range s.membership[user] {
		if id == groupID {
			return false, nil
		}
	}
	s.membership[user] = append(s.membership[user], groupID)
	s.writes = append(s.writes, "add "+user+" to "+group.Name)
	return true, nil
}

func (s *fakeUserGroupStore) DeleteUserFromGroupID(ctx context.Context, groupID int, user string) error {
	kept := make([]int, 0, len(s.membership[user]))
	for _, id := range s.membership[user] {
		if id == groupID {
			group, _ := s.findGroupID(id)
			s.writes = append(s.writes, "delete "+user+" from "+group.Name)
			continue
		}
		kept = append(kept, id)
	}
	s.membership[user] = kept
	return nil
}

func newTestHeaderAuthenticator(config HeaderAuthenticatorConfig, store userGroupStore) *headerAuthenticator {
	auth := NewHeaderAuthenticator(config)
	auth.groups = store
	return auth
}

func newTestRequest(headers map[string]string) *http.Request {
	req := httptest.NewRequest("GET", "/whoami", nil)
	req.RemoteAddr = "192.0.2.10:34567"
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	return req
}

func TestParseGroups(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix string
		raw    string
		want   []string
	}{
		{
			name:   "the prefix is cut off and the other groups are ignored",
			prefix: "/onlineconf/",
			raw:    "/onlineconf/back-developer,/maintainers,/onlineconf/root",
			want:   []string{"back-developer", "root"},
		},
		{
			name:   "the spaces around the groups are trimmed",
			prefix: "/onlineconf/",
			raw:    " /onlineconf/a ,\t/onlineconf/b",
			want:   []string{"a", "b"},
		},
		{
			name:   "an empty value gives no groups",
			prefix: "/onlineconf/",
			raw:    "",
			want:   []string{},
		},
		{
			name:   "the empty fields and the bare prefix are skipped",
			prefix: "/onlineconf/",
			raw:    ",/onlineconf/,  ,/onlineconf/a,",
			want:   []string{"a"},
		},
		{
			name:   "a nested group is skipped",
			prefix: "/onlineconf/",
			raw:    "/onlineconf/a/b,/onlineconf/a",
			want:   []string{"a"},
		},
		{
			name:   "a too long name is skipped",
			prefix: "/onlineconf/",
			raw:    "/onlineconf/" + strings.Repeat("x", maxGroupNameLength+1) + ",/onlineconf/" + strings.Repeat("y", maxGroupNameLength),
			want:   []string{strings.Repeat("y", maxGroupNameLength)},
		},
		{
			name:   "the repetitions of a name are collapsed",
			prefix: "/onlineconf/",
			raw:    "/onlineconf/a,/onlineconf/a",
			want:   []string{"a"},
		},
		{
			// The database tells whether the two address the same group, see
			// resolveGroups.
			name:   "the names differing in the case are both kept",
			prefix: "/onlineconf/",
			raw:    "/onlineconf/a,/onlineconf/A",
			want:   []string{"a", "A"},
		},
		{
			name:   "without a prefix the names are taken as is",
			prefix: "",
			raw:    "a, b",
			want:   []string{"a", "b"},
		},
		{
			name:   "without a prefix a full Keycloak path is skipped as nested",
			prefix: "",
			raw:    "/maintainers,plain",
			want:   []string{"plain"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := newTestHeaderAuthenticator(HeaderAuthenticatorConfig{
				GroupsHeader: "X-Forwarded-Groups",
				GroupsPrefix: tc.prefix,
			}, newFakeUserGroupStore(nil, nil))
			got := auth.parseGroups(context.Background(), tc.raw)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseGroups(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestAuthenticateWithoutGroupsHeaderConfigured checks the backward
// compatibility: without groups_header nothing is read from the groups header
// and nothing is written to the database.
func TestAuthenticateWithoutGroupsHeaderConfigured(t *testing.T) {
	store := newFakeUserGroupStore([]string{"root"}, map[string][]string{"alice": {"root"}})
	auth := newTestHeaderAuthenticator(HeaderAuthenticatorConfig{}, store)

	username, err := auth.Authenticate(newTestRequest(map[string]string{
		"X-Username":         "alice",
		"X-Forwarded-Groups": "/onlineconf/new-group",
	}))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if username != "alice" {
		t.Errorf("username = %q, want alice", username)
	}
	if len(store.writes) != 0 {
		t.Errorf("the database is written: %q", store.writes)
	}
	if !reflect.DeepEqual(store.groupNames(), []string{"root"}) {
		t.Errorf("groups = %q, want [root]", store.groupNames())
	}
}

// TestAuthenticateCreatesGroupAndMembership checks that the first request of a
// user carrying an unknown group creates the group without rights and adds the
// user to it, and that a group without the prefix is not created.
func TestAuthenticateCreatesGroupAndMembership(t *testing.T) {
	store := newFakeUserGroupStore(nil, nil)
	auth := newTestHeaderAuthenticator(HeaderAuthenticatorConfig{
		GroupsHeader: "X-Forwarded-Groups",
		GroupsPrefix: "/onlineconf/",
	}, store)

	username, err := auth.Authenticate(newTestRequest(map[string]string{
		"X-Username":         "alice",
		"X-Forwarded-Groups": "/onlineconf/new-group,/maintainers",
	}))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if username != "alice" {
		t.Errorf("username = %q, want alice", username)
	}
	if !reflect.DeepEqual(store.groupNames(), []string{"new-group"}) {
		t.Errorf("groups = %q, want [new-group]", store.groupNames())
	}
	if !reflect.DeepEqual(store.userGroups("alice"), []string{"new-group"}) {
		t.Errorf("membership = %q, want [new-group]", store.userGroups("alice"))
	}
	want := []string{"create group new-group", "add alice to new-group"}
	if !reflect.DeepEqual(store.writes, want) {
		t.Errorf("writes = %q, want %q", store.writes, want)
	}
}

// TestAuthenticateRemovesMembership checks that a group dropped from the header
// takes the user out of it, while the group itself survives.
func TestAuthenticateRemovesMembership(t *testing.T) {
	store := newFakeUserGroupStore([]string{"root", "back-developer"}, map[string][]string{
		"alice": {"root", "back-developer"},
	})
	auth := newTestHeaderAuthenticator(HeaderAuthenticatorConfig{
		GroupsHeader: "X-Forwarded-Groups",
		GroupsPrefix: "/onlineconf/",
	}, store)

	if _, err := auth.Authenticate(newTestRequest(map[string]string{
		"X-Username":         "alice",
		"X-Forwarded-Groups": "/onlineconf/back-developer",
	})); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !reflect.DeepEqual(store.userGroups("alice"), []string{"back-developer"}) {
		t.Errorf("membership = %q, want [back-developer]", store.userGroups("alice"))
	}
	if !reflect.DeepEqual(store.groupNames(), []string{"root", "back-developer"}) {
		t.Errorf("groups = %q, both groups must survive", store.groupNames())
	}
	want := []string{"delete alice from root"}
	if !reflect.DeepEqual(store.writes, want) {
		t.Errorf("writes = %q, want %q", store.writes, want)
	}
}

// TestAuthenticateRootMembershipFollowsHeader checks the group root is
// synchronized as any other group: Keycloak is the only source of the
// membership.
func TestAuthenticateRootMembershipFollowsHeader(t *testing.T) {
	store := newFakeUserGroupStore([]string{"root"}, map[string][]string{
		"alice": {"root"},
	})
	auth := newTestHeaderAuthenticator(HeaderAuthenticatorConfig{
		GroupsHeader: "X-Forwarded-Groups",
		GroupsPrefix: "/onlineconf/",
	}, store)

	// alice is in root but Keycloak does not say so any more: she loses root.
	if _, err := auth.Authenticate(newTestRequest(map[string]string{
		"X-Username":         "alice",
		"X-Forwarded-Groups": "/onlineconf/back-developer",
	})); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if groups := store.userGroups("alice"); !reflect.DeepEqual(groups, []string{"back-developer"}) {
		t.Errorf("membership of alice = %q, want [back-developer]", groups)
	}

	// bob is in no group and Keycloak puts him into root: he gets root.
	if _, err := auth.Authenticate(newTestRequest(map[string]string{
		"X-Username":         "bob",
		"X-Forwarded-Groups": "/onlineconf/root",
	})); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if groups := store.userGroups("bob"); !reflect.DeepEqual(groups, []string{"root"}) {
		t.Errorf("membership of bob = %q, want [root]", groups)
	}
}

// TestAuthenticateWithoutGroupsHeaderInRequest checks that a request carrying
// no groups header does not change the membership, so that the emergency login
// keeps working.
func TestAuthenticateWithoutGroupsHeaderInRequest(t *testing.T) {
	store := newFakeUserGroupStore([]string{"root"}, map[string][]string{"alice": {"root"}})
	auth := newTestHeaderAuthenticator(HeaderAuthenticatorConfig{
		GroupsHeader: "X-Forwarded-Groups",
		GroupsPrefix: "/onlineconf/",
	}, store)

	username, err := auth.Authenticate(newTestRequest(map[string]string{"X-Username": "alice"}))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if username != "alice" {
		t.Errorf("username = %q, want alice", username)
	}
	if len(store.writes) != 0 {
		t.Errorf("the database is written: %q", store.writes)
	}
	if !reflect.DeepEqual(store.userGroups("alice"), []string{"root"}) {
		t.Errorf("membership = %q, want [root]", store.userGroups("alice"))
	}
}

// TestAuthenticateEmptyGroupsHeaderInRequest checks that an empty groups header
// is not the same as no groups header: it drops every group of the user.
func TestAuthenticateEmptyGroupsHeaderInRequest(t *testing.T) {
	store := newFakeUserGroupStore([]string{"root"}, map[string][]string{"alice": {"root"}})
	auth := newTestHeaderAuthenticator(HeaderAuthenticatorConfig{
		GroupsHeader: "X-Forwarded-Groups",
		GroupsPrefix: "/onlineconf/",
	}, store)

	if _, err := auth.Authenticate(newTestRequest(map[string]string{
		"X-Username":         "alice",
		"X-Forwarded-Groups": "",
	})); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if groups := store.userGroups("alice"); len(groups) != 0 {
		t.Errorf("membership = %q, want no groups", groups)
	}
}

// TestAuthenticateWritesOnDivergenceOnly checks the database is not written
// when the membership already matches the header.
func TestAuthenticateWritesOnDivergenceOnly(t *testing.T) {
	store := newFakeUserGroupStore([]string{"root", "back-developer"}, map[string][]string{
		"alice": {"back-developer", "root"},
	})
	auth := newTestHeaderAuthenticator(HeaderAuthenticatorConfig{
		GroupsHeader: "X-Forwarded-Groups",
		GroupsPrefix: "/onlineconf/",
	}, store)

	for i := 0; i < 2; i++ {
		if _, err := auth.Authenticate(newTestRequest(map[string]string{
			"X-Username":         "alice",
			"X-Forwarded-Groups": "/onlineconf/root,/onlineconf/back-developer",
		})); err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
	}
	if len(store.writes) != 0 {
		t.Errorf("the database is written: %q", store.writes)
	}
}

// TestAuthenticateGroupNameDiffersFromDatabase checks that a name of the header
// that differs from the name stored in the database and still is the same name
// for it neither duplicates the group nor rewrites the membership: the
// collation of my_config_group.Name ignores the case and the accents.
//
// This is the regression test of the membership flapping on every request: the
// names used to be compared in Go by strings.ToLower, which told cafe and café
// apart while the database did not, so every request added the user to the
// group it already was in (a no-op) and removed it from the group of the other
// spelling (the very same membership).
func TestAuthenticateGroupNameDiffersFromDatabase(t *testing.T) {
	for _, tc := range []struct{ name, stored, sent string }{
		{name: "the case differs", stored: "root", sent: "Root"},
		{name: "the accent is in the database only", stored: "café", sent: "cafe"},
		{name: "the accent is in the header only", stored: "cafe", sent: "café"},
		{name: "ё and е differ", stored: "группа-ё", sent: "группа-е"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeUserGroupStore([]string{tc.stored}, map[string][]string{"alice": {tc.stored}})
			auth := newTestHeaderAuthenticator(HeaderAuthenticatorConfig{
				GroupsHeader: "X-Forwarded-Groups",
				GroupsPrefix: "/onlineconf/",
			}, store)

			// Several requests in a row: the flapping took the user out of the
			// group on every second one of them.
			for i := 0; i < 3; i++ {
				if _, err := auth.Authenticate(newTestRequest(map[string]string{
					"X-Username":         "alice",
					"X-Forwarded-Groups": "/onlineconf/" + tc.sent,
				})); err != nil {
					t.Fatalf("Authenticate: %v", err)
				}
				if groups := store.userGroups("alice"); !reflect.DeepEqual(groups, []string{tc.stored}) {
					t.Fatalf("groups of alice after the request %d = %q, want [%s]", i+1, groups, tc.stored)
				}
			}
			if len(store.writes) != 0 {
				t.Errorf("the database is written: %q", store.writes)
			}
			if !reflect.DeepEqual(store.groupNames(), []string{tc.stored}) {
				t.Errorf("groups = %q, want [%s]: the group must not be duplicated", store.groupNames(), tc.stored)
			}
		})
	}
}

// TestAuthenticateGroupsOfTheHeaderAddressOneGroup checks that two names of the
// groups header that are the same name for the database create a single group
// and a single membership, and that the next request writes nothing.
func TestAuthenticateGroupsOfTheHeaderAddressOneGroup(t *testing.T) {
	store := newFakeUserGroupStore(nil, nil)
	auth := newTestHeaderAuthenticator(HeaderAuthenticatorConfig{
		GroupsHeader: "X-Forwarded-Groups",
		GroupsPrefix: "/onlineconf/",
	}, store)

	for i := 0; i < 2; i++ {
		if _, err := auth.Authenticate(newTestRequest(map[string]string{
			"X-Username":         "alice",
			"X-Forwarded-Groups": "/onlineconf/cafe,/onlineconf/café",
		})); err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		if groups := store.userGroups("alice"); !reflect.DeepEqual(groups, []string{"cafe"}) {
			t.Fatalf("groups of alice after the request %d = %q, want [cafe]", i+1, groups)
		}
		if names := store.groupNames(); !reflect.DeepEqual(names, []string{"cafe"}) {
			t.Fatalf("groups after the request %d = %q, want [cafe]", i+1, names)
		}
	}
	want := []string{"create group cafe", "add alice to cafe"}
	if !reflect.DeepEqual(store.writes, want) {
		t.Errorf("writes = %q, want %q", store.writes, want)
	}
}

// TestAuthenticateReturnsStoreError checks a failure of the synchronization is
// reported as an error and not as a successful authentication.
func TestAuthenticateReturnsStoreError(t *testing.T) {
	store := newFakeUserGroupStore(nil, nil)
	store.selectErr = context.DeadlineExceeded
	auth := newTestHeaderAuthenticator(HeaderAuthenticatorConfig{
		GroupsHeader: "X-Forwarded-Groups",
		GroupsPrefix: "/onlineconf/",
	}, store)

	username, err := auth.Authenticate(newTestRequest(map[string]string{
		"X-Username":         "alice",
		"X-Forwarded-Groups": "/onlineconf/a",
	}))
	if err == nil {
		t.Fatal("Authenticate returned no error")
	}
	if username != "" {
		t.Errorf("username = %q, want an empty one", username)
	}
}

func signHeaderValue(parts ...string) string {
	h := md5.New()
	for _, part := range parts {
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TestAuthenticateSignWithoutGroupsHeaderConfigured checks the historical
// format of the signature keeps working without groups_header.
func TestAuthenticateSignWithoutGroupsHeaderConfigured(t *testing.T) {
	store := newFakeUserGroupStore(nil, nil)
	auth := newTestHeaderAuthenticator(HeaderAuthenticatorConfig{Secret: "s3cret"}, store)

	username, err := auth.Authenticate(newTestRequest(map[string]string{
		"X-Username":      "alice",
		"X-Username-Sign": signHeaderValue("alice", "192.0.2.10", "s3cret"),
	}))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if username != "alice" {
		t.Errorf("username = %q, want alice", username)
	}
}

// TestAuthenticateSignCoversGroups checks the signature of a request carrying
// the groups header covers the groups: the request is accepted with the
// matching signature only, and the groups cannot be replaced or stripped
// keeping the signature of the proxy.
func TestAuthenticateSignCoversGroups(t *testing.T) {
	const (
		secret = "s3cret"
		ip     = "192.0.2.10"
		groups = "/onlineconf/back-developer"
	)
	validSign := signHeaderValue("alice\n+"+groups+"\n", ip, secret)

	for _, tc := range []struct {
		name     string
		headers  map[string]string
		username string
	}{
		{
			name: "the signature of the proxy is accepted",
			headers: map[string]string{
				"X-Username":         "alice",
				"X-Forwarded-Groups": groups,
				"X-Username-Sign":    validSign,
			},
			username: "alice",
		},
		{
			name: "the groups replaced keeping the signature are rejected",
			headers: map[string]string{
				"X-Username":         "alice",
				"X-Forwarded-Groups": "/onlineconf/root",
				"X-Username-Sign":    validSign,
			},
			username: "",
		},
		{
			name: "the groups header stripped keeping the signature is rejected",
			headers: map[string]string{
				"X-Username":      "alice",
				"X-Username-Sign": validSign,
			},
			username: "",
		},
		{
			name: "the historical signature without the groups is rejected",
			headers: map[string]string{
				"X-Username":         "alice",
				"X-Forwarded-Groups": groups,
				"X-Username-Sign":    signHeaderValue("alice", ip, secret),
			},
			username: "",
		},
		{
			name: "a request without the groups header is accepted with its own signature",
			headers: map[string]string{
				"X-Username":      "alice",
				"X-Username-Sign": signHeaderValue("alice\n-\n", ip, secret),
			},
			username: "alice",
		},
		{
			name: "a request with an empty groups header is accepted with its own signature",
			headers: map[string]string{
				"X-Username":         "alice",
				"X-Forwarded-Groups": "",
				"X-Username-Sign":    signHeaderValue("alice\n+\n", ip, secret),
			},
			username: "alice",
		},
		{
			name: "a signature of another user is rejected",
			headers: map[string]string{
				"X-Username":         "bob",
				"X-Forwarded-Groups": groups,
				"X-Username-Sign":    validSign,
			},
			username: "",
		},
		{
			name: "a missing signature is rejected",
			headers: map[string]string{
				"X-Username":         "alice",
				"X-Forwarded-Groups": groups,
			},
			username: "",
		},
		{
			name: "a signature that is not hex is rejected",
			headers: map[string]string{
				"X-Username":         "alice",
				"X-Forwarded-Groups": groups,
				"X-Username-Sign":    "not a hex",
			},
			username: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeUserGroupStore([]string{"root"}, map[string][]string{"alice": {"root"}})
			auth := newTestHeaderAuthenticator(HeaderAuthenticatorConfig{
				Secret:       secret,
				GroupsHeader: "X-Forwarded-Groups",
				GroupsPrefix: "/onlineconf/",
			}, store)

			username, err := auth.Authenticate(newTestRequest(tc.headers))
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if username != tc.username {
				t.Errorf("username = %q, want %q", username, tc.username)
			}
			if username == "" && len(store.writes) != 0 {
				t.Errorf("a rejected request has written the database: %q", store.writes)
			}
		})
	}
}

// TestAuthenticateSeveralGroupsHeaders checks the groups header sent in several
// lines is taken as a whole: by RFC 9110 §5.3 the lines are equivalent to a
// single line holding the values separated by commas, so no group is lost if
// the proxy sends a line per group.
func TestAuthenticateSeveralGroupsHeaders(t *testing.T) {
	store := newFakeUserGroupStore([]string{"root", "back-developer"}, nil)
	auth := newTestHeaderAuthenticator(HeaderAuthenticatorConfig{
		GroupsHeader: "X-Forwarded-Groups",
		GroupsPrefix: "/onlineconf/",
	}, store)

	req := newTestRequest(map[string]string{"X-Username": "alice"})
	req.Header.Add("X-Forwarded-Groups", "/onlineconf/back-developer")
	req.Header.Add("X-Forwarded-Groups", "/onlineconf/root")

	if _, err := auth.Authenticate(req); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if groups := store.userGroups("alice"); !reflect.DeepEqual(groups, []string{"back-developer", "root"}) {
		t.Errorf("membership = %q, want [back-developer root]", groups)
	}
}

// TestAuthenticateSignCoversEveryGroupsHeaderLine checks the signature of a
// request carrying the groups header in several lines covers all of them, so
// that an appended line cannot add a group.
func TestAuthenticateSignCoversEveryGroupsHeaderLine(t *testing.T) {
	const (
		secret = "s3cret"
		ip     = "192.0.2.10"
	)
	for _, tc := range []struct {
		name     string
		lines    []string
		sign     string
		username string
	}{
		{
			name:     "the lines joined with a comma are signed",
			lines:    []string{"/onlineconf/back-developer", "/onlineconf/root"},
			sign:     signHeaderValue("alice\n+/onlineconf/back-developer,/onlineconf/root\n", ip, secret),
			username: "alice",
		},
		{
			name:     "a line appended to the signed one is rejected",
			lines:    []string{"/onlineconf/back-developer", "/onlineconf/root"},
			sign:     signHeaderValue("alice\n+/onlineconf/back-developer\n", ip, secret),
			username: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeUserGroupStore([]string{"root", "back-developer"}, nil)
			auth := newTestHeaderAuthenticator(HeaderAuthenticatorConfig{
				Secret:       secret,
				GroupsHeader: "X-Forwarded-Groups",
				GroupsPrefix: "/onlineconf/",
			}, store)

			req := newTestRequest(map[string]string{
				"X-Username":      "alice",
				"X-Username-Sign": tc.sign,
			})
			for _, line := range tc.lines {
				req.Header.Add("X-Forwarded-Groups", line)
			}

			username, err := auth.Authenticate(req)
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if username != tc.username {
				t.Errorf("username = %q, want %q", username, tc.username)
			}
			if username == "" && len(store.writes) != 0 {
				t.Errorf("a rejected request has written the database: %q", store.writes)
			}
		})
	}
}
