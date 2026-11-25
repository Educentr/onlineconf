package admin

import (
	"context"

	. "github.com/onlineconf/onlineconf/admin/go/common"
)

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

// SelectUserGroups returns the names of the groups the user belongs to.
func SelectUserGroups(ctx context.Context, user string) ([]string, error) {
	rows, err := DB.QueryContext(ctx, `
		SELECT g.Name
		FROM my_config_group g
		JOIN my_config_user_group ug ON ug.GroupID = g.ID
		WHERE ug.User = ?
		ORDER BY g.Name
	`, user)
	if err != nil {
		return nil, err
	}
	return ReadStrings(rows)
}

func AddUserToGroup(ctx context.Context, group, user string) error {
	_, err := DB.ExecContext(ctx, "INSERT INTO my_config_user_group (GroupID, User) VALUES ((SELECT ID from my_config_group WHERE Name = ?), ?)", group, user)
	return err
}

// AddUserToGroupIfNotExists adds the user to the group and reports whether the
// membership has been created. Unlike AddUserToGroup it does not fail if the
// user is already a member of the group, because parallel requests of the same
// user can add it concurrently. The membership is not created at all if the
// group does not exist.
func AddUserToGroupIfNotExists(ctx context.Context, group, user string) (bool, error) {
	res, err := DB.ExecContext(ctx, `
		INSERT IGNORE INTO my_config_user_group (GroupID, User)
		SELECT ID, ? FROM my_config_group WHERE Name = ?
	`, user, group)
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
