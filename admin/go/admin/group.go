package admin

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	. "github.com/onlineconf/onlineconf/admin/go/common"
)

// Group is a group of users of OnlineConf.
//
// A group is identified by its ID and not by its name, because it is the
// database that decides which names are the same one: the names are compared in
// the collation of my_config_group.Name, which ignores both the case and the
// accents on MySQL 8 (utf8mb4_0900_ai_ci, so root, Root, cafe and café are all
// the same name), and the unique key of the column lets only one group of such
// names exist. Go has no equivalent of that comparison, so the names are never
// compared outside the database: they are resolved to a Group first, see
// SelectGroupsByNames.
type Group struct {
	ID   int
	Name string
}

// readGroups reads the rows of the (ID, Name) pairs of the groups.
func readGroups(rows *sql.Rows) ([]Group, error) {
	defer rows.Close()
	groups := make([]Group, 0)
	for rows.Next() {
		var group Group
		if err := rows.Scan(&group.ID, &group.Name); err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func SelectGroups(ctx context.Context) ([]string, error) {
	rows, err := DB.QueryContext(ctx, "SELECT Name FROM my_config_group ORDER BY Name")
	if err != nil {
		return nil, err
	}
	return ReadStrings(rows)
}

func CreateGroup(ctx context.Context, name string) error {
	_, err := DB.ExecContext(ctx, "INSERT INTO my_config_group (Name) VALUES (?)", name)
	return err
}

// CreateGroupIfNotExists creates a group without any rights on the tree and
// reports whether it has been created. Unlike CreateGroup it does not fail if
// the group already exists, because parallel requests of the same user can
// create the group concurrently.
func CreateGroupIfNotExists(ctx context.Context, name string) (bool, error) {
	res, err := DB.ExecContext(ctx, "INSERT IGNORE INTO my_config_group (Name) VALUES (?)", name)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func DeleteGroup(ctx context.Context, name string) error {
	_, err := DB.ExecContext(ctx, "DELETE FROM my_config_group WHERE Name = ?", name)
	return err
}

func SelectGroupUsers(ctx context.Context, group string) ([]string, error) {
	rows, err := DB.QueryContext(ctx, "SELECT User FROM my_config_user_group WHERE GroupID = (SELECT ID FROM my_config_group WHERE Name = ?) ORDER BY User", group)
	if err != nil {
		return nil, err
	}
	return ReadStrings(rows)
}

// SelectUserGroups returns the groups the user belongs to.
func SelectUserGroups(ctx context.Context, user string) ([]Group, error) {
	rows, err := DB.QueryContext(ctx, `
		SELECT g.ID, g.Name
		FROM my_config_group g
		JOIN my_config_user_group ug ON ug.GroupID = g.ID
		WHERE ug.User = ?
		ORDER BY g.Name
	`, user)
	if err != nil {
		return nil, err
	}
	return readGroups(rows)
}

// SelectGroupsByNames resolves the names to the groups they address. The result
// is aligned with names: its i-th element is the group stored under names[i], or
// nil if the database holds no group of that name.
//
// The names are matched by the database and never in Go, because it is the
// collation of my_config_group.Name that decides which names are the same one
// (see Group). Several names can therefore resolve to one and the same group.
func SelectGroupsByNames(ctx context.Context, names []string) ([]*Group, error) {
	if len(names) == 0 {
		return nil, nil
	}

	// A branch per name instead of a single IN (...) or a join with a derived
	// table of the names: the branch tells which name has matched, and every
	// name is compared with the column directly, so the comparison is made in
	// the collation of the column. A name taken from a derived table would be a
	// column of the collation of the connection instead (utf8mb4_general_ci,
	// see MysqlInitConfig), and MySQL refuses to compare two columns of
	// different collations at all.
	var query strings.Builder
	bind := make([]interface{}, 0, len(names))
	for i, name := range names {
		if i > 0 {
			query.WriteString(" UNION ALL ")
		}
		fmt.Fprintf(&query, "SELECT %d AS Idx, ID, Name FROM my_config_group WHERE Name = ?", i)
		bind = append(bind, name)
	}

	rows, err := DB.QueryContext(ctx, query.String(), bind...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	resolved := make([]*Group, len(names))
	for rows.Next() {
		var idx int
		var group Group
		if err := rows.Scan(&idx, &group.ID, &group.Name); err != nil {
			return nil, err
		}
		resolved[idx] = &group
	}
	return resolved, rows.Err()
}

func AddUserToGroup(ctx context.Context, group, user string) error {
	_, err := DB.ExecContext(ctx, "INSERT INTO my_config_user_group (GroupID, User) VALUES ((SELECT ID from my_config_group WHERE Name = ?), ?)", group, user)
	return err
}

// AddUserToGroupIDIfNotExists adds the user to the group identified by its ID
// and reports whether the membership has been created. Unlike AddUserToGroup it
// does not fail if the user is already a member of the group, because parallel
// requests of the same user can add it concurrently. The membership is not
// created at all if the group has been deleted meanwhile.
func AddUserToGroupIDIfNotExists(ctx context.Context, groupID int, user string) (bool, error) {
	res, err := DB.ExecContext(ctx, `
		INSERT IGNORE INTO my_config_user_group (GroupID, User)
		SELECT ID, ? FROM my_config_group WHERE ID = ?
	`, user, groupID)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func DeleteUserFromGroup(ctx context.Context, group, user string) error {
	_, err := DB.ExecContext(ctx, "DELETE FROM my_config_user_group WHERE GroupID = (SELECT ID FROM my_config_group WHERE Name = ?) AND User = ?", group, user)
	return err
}

// DeleteUserFromGroupID removes the user from the group identified by its ID.
func DeleteUserFromGroupID(ctx context.Context, groupID int, user string) error {
	_, err := DB.ExecContext(ctx, "DELETE FROM my_config_user_group WHERE GroupID = ? AND User = ?", groupID, user)
	return err
}

func UserIsRoot(ctx context.Context, user string) (bool, error) {
	row := DB.QueryRowContext(ctx, `
		SELECT count(*)
		FROM my_config_group g
		JOIN my_config_user_group ug ON ug.GroupID = g.ID
		WHERE g.Name = 'root' AND ug.User = ?
	`, user)
	var count int
	err := row.Scan(&count)
	return count > 0, err
}
