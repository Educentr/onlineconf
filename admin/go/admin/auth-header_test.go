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

// fakeUserGroupStore is an in-memory userGroupStore recording every write.
type fakeUserGroupStore struct {
	groups     []string            // the groups existing in the "database"
	membership map[string][]string // user -> the groups of the user
	writes     []string            // the writes performed, in order
	selectErr  error
}

func newFakeUserGroupStore(groups []string, membership map[string][]string) *fakeUserGroupStore {
	if membership == nil {
		membership = map[string][]string{}
	}
	return &fakeUserGroupStore{groups: groups, membership: membership}
}

// findGroup returns the existing name of the group matching name
// case-insensitively, the way the unique key of my_config_group.Name does.
func (s *fakeUserGroupStore) findGroup(name string) (string, bool) {
	for _, group := range s.groups {
		if strings.EqualFold(group, name) {
			return group, true
		}
	}
	return "", false
}

func (s *fakeUserGroupStore) SelectUserGroups(ctx context.Context, user string) ([]string, error) {
	if s.selectErr != nil {
		return nil, s.selectErr
	}
	groups := append([]string{}, s.membership[user]...)
	sort.Strings(groups)
	return groups, nil
}

func (s *fakeUserGroupStore) CreateGroupIfNotExists(ctx context.Context, name string) (bool, error) {
	if _, ok := s.findGroup(name); ok {
		return false, nil
	}
	s.groups = append(s.groups, name)
	s.writes = append(s.writes, "create group "+name)
	return true, nil
}

func (s *fakeUserGroupStore) AddUserToGroupIfNotExists(ctx context.Context, group, user string) (bool, error) {
	existing, ok := s.findGroup(group)
	if !ok {
		return false, nil
	}
	for _, member := range s.membership[user] {
		if strings.EqualFold(member, existing) {
			return false, nil
		}
	}
	s.membership[user] = append(s.membership[user], existing)
	s.writes = append(s.writes, "add "+user+" to "+existing)
	return true, nil
}

func (s *fakeUserGroupStore) DeleteUserFromGroup(ctx context.Context, group, user string) error {
	kept := make([]string, 0, len(s.membership[user]))
	for _, member := range s.membership[user] {
		if strings.EqualFold(member, group) {
			s.writes = append(s.writes, "delete "+user+" from "+member)
			continue
		}
		kept = append(kept, member)
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
			name:   "the duplicates are collapsed ignoring the case",
			prefix: "/onlineconf/",
			raw:    "/onlineconf/a,/onlineconf/a,/onlineconf/A",
			want:   []string{"a"},
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
	if !reflect.DeepEqual(store.groups, []string{"root"}) {
		t.Errorf("groups = %q, want [root]", store.groups)
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
	if !reflect.DeepEqual(store.groups, []string{"new-group"}) {
		t.Errorf("groups = %q, want [new-group]", store.groups)
	}
	if !reflect.DeepEqual(store.membership["alice"], []string{"new-group"}) {
		t.Errorf("membership = %q, want [new-group]", store.membership["alice"])
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
	if !reflect.DeepEqual(store.membership["alice"], []string{"back-developer"}) {
		t.Errorf("membership = %q, want [back-developer]", store.membership["alice"])
	}
	if !reflect.DeepEqual(store.groups, []string{"root", "back-developer"}) {
		t.Errorf("groups = %q, both groups must survive", store.groups)
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
	if groups := store.membership["alice"]; !reflect.DeepEqual(groups, []string{"back-developer"}) {
		t.Errorf("membership of alice = %q, want [back-developer]", groups)
	}

	// bob is in no group and Keycloak puts him into root: he gets root.
	if _, err := auth.Authenticate(newTestRequest(map[string]string{
		"X-Username":         "bob",
		"X-Forwarded-Groups": "/onlineconf/root",
	})); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if groups := store.membership["bob"]; !reflect.DeepEqual(groups, []string{"root"}) {
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
	if !reflect.DeepEqual(store.membership["alice"], []string{"root"}) {
		t.Errorf("membership = %q, want [root]", store.membership["alice"])
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
	if groups := store.membership["alice"]; len(groups) != 0 {
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

// TestAuthenticateGroupCaseDiffersFromDatabase checks that a group differing
// from the group of the database in case only is not rewritten on every
// request: the unique key of my_config_group.Name is case-insensitive, so both
// names address the same group.
func TestAuthenticateGroupCaseDiffersFromDatabase(t *testing.T) {
	store := newFakeUserGroupStore([]string{"root"}, map[string][]string{"alice": {"root"}})
	auth := newTestHeaderAuthenticator(HeaderAuthenticatorConfig{
		GroupsHeader: "X-Forwarded-Groups",
		GroupsPrefix: "/onlineconf/",
	}, store)

	if _, err := auth.Authenticate(newTestRequest(map[string]string{
		"X-Username":         "alice",
		"X-Forwarded-Groups": "/onlineconf/Root",
	})); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if len(store.writes) != 0 {
		t.Errorf("the database is written: %q", store.writes)
	}
	if !reflect.DeepEqual(store.groups, []string{"root"}) {
		t.Errorf("groups = %q, want [root]", store.groups)
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

// TestAuthenticateSeveralGroupsHeaders checks only the first occurrence of the
// groups header is honoured, so that an appended one cannot add a group.
func TestAuthenticateSeveralGroupsHeaders(t *testing.T) {
	store := newFakeUserGroupStore([]string{"root"}, nil)
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
	if groups := store.membership["alice"]; !reflect.DeepEqual(groups, []string{"back-developer"}) {
		t.Errorf("membership = %q, want [back-developer]", groups)
	}
}
