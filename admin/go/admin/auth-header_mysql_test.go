package admin

// The tests of this file run the synchronization of the groups against a real
// MySQL, because its result is read back by the SQL functions of OnlineConf
// (my_config_tree_access) and depends on the collation and on the unique keys
// of the schema. They are skipped unless ONLINECONF_TEST_MYSQL_DSN points at a
// database with admin/etc/schema.sql loaded. The tests truncate the tables of
// the groups and of the tree, so the database must be a throwaway one:
//
//	docker run -d --name oc-test-mysql \
//	    -e MYSQL_RANDOM_ROOT_PASSWORD=1 -e MYSQL_DATABASE=onlineconf \
//	    -e MYSQL_USER=onlineconf -e MYSQL_PASSWORD=onlineconf -p 13306:3306 \
//	    mysql:8.0 --character-set-server=utf8mb4 --log-bin-trust-function-creators=1
//	until docker exec oc-test-mysql mysqladmin ping --silent; do sleep 1; done
//	docker exec -i oc-test-mysql mysql -uonlineconf -ponlineconf onlineconf < admin/etc/schema.sql
//	ONLINECONF_TEST_MYSQL_DSN='onlineconf:onlineconf@tcp(127.0.0.1:13306)/onlineconf?charset=utf8mb4&collation=utf8mb4_general_ci' \
//	    go test ./admin/ -run MySQL -v

import (
	"context"
	"database/sql"
	"os"
	"reflect"
	"sync"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	. "github.com/onlineconf/onlineconf/admin/go/common"
)

// openTestDatabase connects to the throwaway database, assigns it to the DB the
// package works with and truncates the tables the tests touch.
func openTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("ONLINECONF_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("ONLINECONF_TEST_MYSQL_DSN is not set, see the comment at the top of auth-header_mysql_test.go")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping the test database: %v", err)
	}

	savedDB := DB
	DB = db
	t.Cleanup(func() {
		DB = savedDB
		db.Close()
	})

	// The cleanup runs on a single connection, because the foreign keys are
	// switched off per session and my_config_tree references itself.
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("connect to the test database: %v", err)
	}
	defer conn.Close()
	for _, query := range []string{
		"SET FOREIGN_KEY_CHECKS = 0",
		"DELETE FROM my_config_user_group",
		"DELETE FROM my_config_tree_group",
		"DELETE FROM my_config_group",
		"DELETE FROM my_config_tree_log",
		"DELETE FROM my_config_tree",
		"SET FOREIGN_KEY_CHECKS = 1",
	} {
		if _, err := conn.ExecContext(context.Background(), query); err != nil {
			t.Fatalf("clean the test database (%s): %v", query, err)
		}
	}
	return db
}

func newMySQLHeaderAuthenticator() *headerAuthenticator {
	return NewHeaderAuthenticator(HeaderAuthenticatorConfig{
		GroupsHeader: "X-Forwarded-Groups",
		GroupsPrefix: "/onlineconf/",
	})
}

// authenticateWithGroups runs the authentication of a request carrying the
// groups header and returns the groups of the user read back from the database.
func authenticateWithGroups(t *testing.T, auth *headerAuthenticator, user, groups string) []string {
	t.Helper()
	req := newTestRequest(map[string]string{
		"X-Username":         user,
		"X-Forwarded-Groups": groups,
	})
	username, err := auth.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if username != user {
		t.Fatalf("username = %q, want %q", username, user)
	}
	current, err := SelectUserGroups(context.Background(), user)
	if err != nil {
		t.Fatalf("SelectUserGroups: %v", err)
	}
	return current
}

func selectGroupNames(t *testing.T) []string {
	t.Helper()
	names, err := SelectGroups(context.Background())
	if err != nil {
		t.Fatalf("SelectGroups: %v", err)
	}
	return names
}

// TestMySQLSyncGroupsCreatesAndRemoves walks through the whole life cycle of
// the membership driven by the groups header.
func TestMySQLSyncGroupsCreatesAndRemoves(t *testing.T) {
	db := openTestDatabase(t)
	auth := newMySQLHeaderAuthenticator()

	if _, err := db.Exec("INSERT INTO my_config_tree (Name, ParentID) VALUES ('', NULL)"); err != nil {
		t.Fatalf("create the root of the tree: %v", err)
	}

	// The first request creates the group of the header and nothing else.
	groups := authenticateWithGroups(t, auth, "alice", "/onlineconf/new-group,/maintainers")
	if !reflect.DeepEqual(groups, []string{"new-group"}) {
		t.Errorf("groups of alice = %q, want [new-group]", groups)
	}
	if names := selectGroupNames(t); !reflect.DeepEqual(names, []string{"new-group"}) {
		t.Errorf("groups = %q, want [new-group]: a group without the prefix must not be created", names)
	}

	// The created group has no rights on the tree.
	var rights int
	if err := DB.QueryRow("SELECT count(*) FROM my_config_tree_group").Scan(&rights); err != nil {
		t.Fatalf("count the rights: %v", err)
	}
	if rights != 0 {
		t.Errorf("the created group has %d rights on the tree, want none", rights)
	}

	// A second group is added keeping the first one.
	groups = authenticateWithGroups(t, auth, "alice", "/onlineconf/new-group,/onlineconf/root")
	if !reflect.DeepEqual(groups, []string{"new-group", "root"}) {
		t.Errorf("groups of alice = %q, want [new-group root]", groups)
	}

	// A group dropped from the header takes alice out of it, the group itself
	// and its rights survive.
	if err := SetAccess(context.Background(), "/", "new-group", NullBool{NullBool: sql.NullBool{Bool: true, Valid: true}}); err != nil {
		t.Fatalf("SetAccess: %v", err)
	}
	groups = authenticateWithGroups(t, auth, "alice", "/onlineconf/root")
	if !reflect.DeepEqual(groups, []string{"root"}) {
		t.Errorf("groups of alice = %q, want [root]", groups)
	}
	if names := selectGroupNames(t); !reflect.DeepEqual(names, []string{"new-group", "root"}) {
		t.Errorf("groups = %q, want [new-group root]: the groups must survive", names)
	}
	if err := DB.QueryRow("SELECT count(*) FROM my_config_tree_group").Scan(&rights); err != nil {
		t.Fatalf("count the rights: %v", err)
	}
	if rights != 1 {
		t.Errorf("the rights of the group are lost: %d rows, want 1", rights)
	}
}

// TestMySQLSyncGroupsRootFollowsHeader checks the group root is synchronized as
// any other group and that UserIsRoot, which the API checks the privileged
// operations by, follows the header.
func TestMySQLSyncGroupsRootFollowsHeader(t *testing.T) {
	openTestDatabase(t)
	ctx := context.Background()
	auth := newMySQLHeaderAuthenticator()

	// Everybody sits in root before the groups header is turned on.
	if err := CreateGroup(ctx, "root"); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if err := AddUserToGroup(ctx, "root", "alice"); err != nil {
		t.Fatalf("AddUserToGroup: %v", err)
	}

	// Keycloak does not say alice is root, so she is not root any more.
	authenticateWithGroups(t, auth, "alice", "/onlineconf/back-developer")
	isRoot, err := UserIsRoot(ctx, "alice")
	if err != nil {
		t.Fatalf("UserIsRoot: %v", err)
	}
	if isRoot {
		t.Error("alice is still root without /onlineconf/root in the header")
	}

	// Keycloak says bob is root, so he becomes root.
	authenticateWithGroups(t, auth, "bob", "/onlineconf/root")
	isRoot, err = UserIsRoot(ctx, "bob")
	if err != nil {
		t.Fatalf("UserIsRoot: %v", err)
	}
	if !isRoot {
		t.Error("bob is not root with /onlineconf/root in the header")
	}
}

// TestMySQLSyncGroupsDrivesTreeAccess checks the synchronized membership is
// what the SQL functions checking the access to the tree read: the rights are
// granted to a group in the UI and are gained and lost with the membership.
func TestMySQLSyncGroupsDrivesTreeAccess(t *testing.T) {
	db := openTestDatabase(t)
	ctx := context.Background()
	auth := newMySQLHeaderAuthenticator()

	if _, err := db.Exec("INSERT INTO my_config_tree (Name, ParentID) VALUES ('', NULL)"); err != nil {
		t.Fatalf("create the root of the tree: %v", err)
	}

	authenticateWithGroups(t, auth, "alice", "/onlineconf/back-developer")
	if err := SetAccess(ctx, "/", "back-developer", NullBool{NullBool: sql.NullBool{Bool: true, Valid: true}}); err != nil {
		t.Fatalf("SetAccess: %v", err)
	}

	canEdit, err := UserCanEditAccess(ctx, "alice", "/")
	if err != nil {
		t.Fatalf("UserCanEditAccess: %v", err)
	}
	if !canEdit {
		t.Error("alice cannot edit / while being in the group the rights are granted to")
	}

	// The group is dropped from the header: the rights go away with the
	// membership, the rights of the group itself are untouched.
	authenticateWithGroups(t, auth, "alice", "")
	canEdit, err = UserCanEditAccess(ctx, "alice", "/")
	if err != nil {
		t.Fatalf("UserCanEditAccess: %v", err)
	}
	if canEdit {
		t.Error("alice can still edit / after losing the group")
	}
}

// TestMySQLSyncGroupsCaseInsensitiveName checks a group differing from the
// group of the database in case only is not duplicated and does not make every
// request rewrite the membership: the unique key of my_config_group.Name is
// case-insensitive.
func TestMySQLSyncGroupsCaseInsensitiveName(t *testing.T) {
	openTestDatabase(t)
	ctx := context.Background()
	auth := newMySQLHeaderAuthenticator()

	if err := CreateGroup(ctx, "root"); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	for i := 0; i < 2; i++ {
		groups := authenticateWithGroups(t, auth, "alice", "/onlineconf/Root")
		if !reflect.DeepEqual(groups, []string{"root"}) {
			t.Fatalf("groups of alice = %q, want [root]", groups)
		}
		if names := selectGroupNames(t); !reflect.DeepEqual(names, []string{"root"}) {
			t.Fatalf("groups = %q, want [root]: the group must not be duplicated", names)
		}
	}
}

// TestMySQLSyncGroupsConcurrently checks the synchronization is idempotent: the
// UI sends several requests in parallel on the first login, and all of them
// create the same group and the same membership.
func TestMySQLSyncGroupsConcurrently(t *testing.T) {
	openTestDatabase(t)
	auth := newMySQLHeaderAuthenticator()

	const parallel = 8
	errs := make([]error, parallel)
	var wg sync.WaitGroup
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := newTestRequest(map[string]string{
				"X-Username":         "alice",
				"X-Forwarded-Groups": "/onlineconf/new-group,/onlineconf/root",
			})
			_, errs[i] = auth.Authenticate(req)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("Authenticate of the parallel request %d: %v", i, err)
		}
	}

	groups, err := SelectUserGroups(context.Background(), "alice")
	if err != nil {
		t.Fatalf("SelectUserGroups: %v", err)
	}
	if !reflect.DeepEqual(groups, []string{"new-group", "root"}) {
		t.Errorf("groups of alice = %q, want [new-group root]", groups)
	}
	if names := selectGroupNames(t); !reflect.DeepEqual(names, []string{"new-group", "root"}) {
		t.Errorf("groups = %q, want [new-group root]", names)
	}
}
