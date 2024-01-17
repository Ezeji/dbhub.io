package common

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/sqlitebrowser/dbhub.io/common/config"
	"github.com/sqlitebrowser/dbhub.io/common/database"

	"github.com/aquilax/truncate"
	pgx "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/smtp2go-oss/smtp2go-go"
)

// AnalysisUsersWithDBs returns the list of users with at least one database
func AnalysisUsersWithDBs() (userList map[string]int, err error) {
	dbQuery := `
		SELECT u.user_name, count(*)
		FROM users u, sqlite_databases db
		WHERE u.user_id = db.user_id
		GROUP BY u.user_name`
	rows, err := database.DB.Query(context.Background(), dbQuery)
	if err != nil {
		log.Printf("Database query failed in %s: %v", GetCurrentFunctionName(), err)
		return
	}
	defer rows.Close()
	userList = make(map[string]int)
	for rows.Next() {
		var user string
		var numDBs int
		err = rows.Scan(&user, &numDBs)
		if err != nil {
			log.Printf("Error in %s when getting the list of users with at least one database: %v", GetCurrentFunctionName(), err)
			return nil, err
		}
		userList[user] = numDBs
	}
	return
}

// CheckDBExists checks if a database exists. It does NOT perform any permission checks.
// If an error occurred, the true/false value should be ignored, as only the error value is valid
func CheckDBExists(dbOwner, dbName string) (bool, error) {
	// Query matching databases
	dbQuery := `
		SELECT COUNT(db_id)
		FROM sqlite_databases
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
			)
			AND db_name = $2
			AND is_deleted = false
		LIMIT 1`
	var dbCount int
	err := database.DB.QueryRow(context.Background(), dbQuery, dbOwner, dbName).Scan(&dbCount)
	if err != nil {
		return false, err
	}

	// Return true if the database count is not zero
	return dbCount != 0, nil
}

// CheckDBLive checks if the given database is a live database
func CheckDBLive(dbOwner, dbName string) (isLive bool, liveNode string, err error) {
	// Query matching databases
	dbQuery := `
		SELECT live_db, coalesce(live_node, '')
		FROM sqlite_databases
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
			)
			AND db_name = $2
			AND is_deleted = false
		LIMIT 1`
	err = database.DB.QueryRow(context.Background(), dbQuery, dbOwner, dbName).Scan(&isLive, &liveNode)
	if err != nil {
		return false, "", err
	}
	return
}

// CheckDBID checks if a given database ID is available, and returns its name so the caller can determine if it
// has been renamed.  If an error occurs, the true/false value should be ignored, as only the error value is valid
func CheckDBID(dbOwner string, dbID int64) (avail bool, dbName string, err error) {
	dbQuery := `
		SELECT db_name
		FROM sqlite_databases
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
			)
			AND db_id = $2
			AND is_deleted = false`
	err = database.DB.QueryRow(context.Background(), dbQuery, dbOwner, dbID).Scan(&dbName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			avail = false
		} else {
			log.Printf("Checking if a database exists failed: %v", err)
		}
		return
	}

	// Database exists
	avail = true
	return
}

// databaseID returns the ID number for a given user's database
func databaseID(dbOwner, dbName string) (dbID int, err error) {
	// Retrieve the database id
	dbQuery := `
		SELECT db_id
		FROM sqlite_databases
		WHERE user_id = (
			SELECT user_id
			FROM users
			WHERE lower(user_name) = lower($1))
		AND db_name = $2
		AND is_deleted = false`
	err = database.DB.QueryRow(context.Background(), dbQuery, dbOwner, dbName).Scan(&dbID)
	if err != nil {
		log.Printf("Error looking up database id. Owner: '%s', Database: '%s'. Error: %v",
			SanitiseLogString(dbOwner), SanitiseLogString(dbName), err)
	}
	return
}

// DB4SDefaultList returns a list of 1) users with public databases, 2) along with the logged in users' most recently
// modified database (including their private one(s))
func DB4SDefaultList(loggedInUser string) (UserInfoSlice, error) {
	// Retrieve the list of all users with public databases
	dbQuery := `
		WITH public_dbs AS (
			SELECT db_id, last_modified
			FROM sqlite_databases
			WHERE public = true
			AND is_deleted = false
			ORDER BY last_modified DESC
		), public_users AS (
			SELECT DISTINCT ON (db.user_id) db.user_id, db.last_modified
			FROM public_dbs as pub, sqlite_databases AS db
			WHERE db.db_id = pub.db_id
			ORDER BY db.user_id, db.last_modified DESC
		)
		SELECT user_name, last_modified
		FROM public_users AS pu, users
		WHERE users.user_id = pu.user_id
			AND users.user_name != $1
		ORDER BY last_modified DESC`
	rows, err := database.DB.Query(context.Background(), dbQuery, loggedInUser)
	if err != nil {
		log.Printf("Database query failed: %v", err)
		return nil, err
	}
	defer rows.Close()
	unsorted := make(map[string]UserInfo)
	for rows.Next() {
		var oneRow UserInfo
		err = rows.Scan(&oneRow.Username, &oneRow.LastModified)
		if err != nil {
			log.Printf("Error list of users with public databases: %v", err)
			return nil, err
		}
		unsorted[oneRow.Username] = oneRow
	}

	// Sort the list by last_modified order, from most recent to oldest
	publicList := make(UserInfoSlice, 0, len(unsorted))
	for _, j := range unsorted {
		publicList = append(publicList, j)
	}
	sort.Sort(publicList)

	// Retrieve the last modified timestamp for the most recent database of the logged in user (if they have any)
	dbQuery = `
		WITH u AS (
			SELECT user_id
			FROM users
			WHERE lower(user_name) = lower($1)
		), user_db_list AS (
			SELECT DISTINCT ON (db_id) db_id, last_modified
			FROM sqlite_databases AS db, u
			WHERE db.user_id = u.user_id
			AND is_deleted = false
		), most_recent_user_db AS (
			SELECT udb.last_modified
			FROM user_db_list AS udb
			ORDER BY udb.last_modified DESC
			LIMIT 1
		)
		SELECT last_modified
		FROM most_recent_user_db`
	userRow := UserInfo{Username: loggedInUser}
	rows, err = database.DB.Query(context.Background(), dbQuery, loggedInUser)
	if err != nil {
		log.Printf("Database query failed: %v", err)
		return nil, err
	}
	defer rows.Close()
	userHasDB := false
	for rows.Next() {
		userHasDB = true
		err = rows.Scan(&userRow.LastModified)
		if err != nil {
			log.Printf("Error retrieving database list for user: %v", err)
			return nil, err
		}
	}

	// If the user doesn't have any databases, just return the list of users with public databases
	if !userHasDB {
		return publicList, nil
	}

	// The user does have at least one database, so include them at the top of the list
	completeList := make(UserInfoSlice, 0, len(unsorted)+1)
	completeList = append(completeList, userRow)
	completeList = append(completeList, publicList...)
	return completeList, nil
}

// DBDetails returns the details for a specific database
func DBDetails(DB *SQLiteDBinfo, loggedInUser, dbOwner, dbName, commitID string) (err error) {
	// Check permissions first
	allowed, err := database.CheckDBPermissions(loggedInUser, dbOwner, dbName, false)
	if err != nil {
		return err
	}
	if allowed == false {
		return fmt.Errorf("The requested database doesn't exist")
	}

	// First, we check if the database is a live one.  If it is, we need to do things a bit differently
	isLive, _, err := CheckDBLive(dbOwner, dbName)
	if err != nil {
		return
	}
	if !isLive {
		// * This is a standard database *

		// If no commit ID was supplied, we retrieve the latest one from the default branch
		if commitID == "" {
			commitID, err = DefaultCommit(dbOwner, dbName)
			if err != nil {
				return err
			}
		}

		// Retrieve the database details
		dbQuery := `
			SELECT db.date_created, db.last_modified, db.watchers, db.stars, db.discussions, db.merge_requests,
				$3::text AS commit_id, db.commit_list->$3::text->'tree'->'entries'->0 AS db_entry, db.branches,
				db.release_count, db.contributors, coalesce(db.one_line_description, ''),
				coalesce(db.full_description, 'No full description'), coalesce(db.default_table, ''), db.public,
				coalesce(db.source_url, ''), db.tags, coalesce(db.default_branch, ''), db.live_db,
				coalesce(db.live_node, ''), coalesce(db.live_minio_object_id, '')
			FROM sqlite_databases AS db
			WHERE db.user_id = (
					SELECT user_id
					FROM users
					WHERE lower(user_name) = lower($1)
				)
				AND db.db_name = $2
				AND db.is_deleted = false`

		// Retrieve the requested database details
		err = database.DB.QueryRow(context.Background(), dbQuery, dbOwner, dbName, commitID).Scan(&DB.Info.DateCreated, &DB.Info.RepoModified,
			&DB.Info.Watchers, &DB.Info.Stars, &DB.Info.Discussions, &DB.Info.MRs, &DB.Info.CommitID, &DB.Info.DBEntry,
			&DB.Info.Branches, &DB.Info.Releases, &DB.Info.Contributors, &DB.Info.OneLineDesc, &DB.Info.FullDesc,
			&DB.Info.DefaultTable, &DB.Info.Public, &DB.Info.SourceURL, &DB.Info.Tags, &DB.Info.DefaultBranch,
			&DB.Info.IsLive, &DB.Info.LiveNode, &DB.MinioId)
		if err != nil {
			log.Printf("Error when retrieving database details: %v", err.Error())
			return errors.New("The requested database doesn't exist")
		}
	} else {
		// This is a live database
		dbQuery := `
			SELECT db.date_created, db.last_modified, db.watchers, db.stars, db.discussions, coalesce(db.one_line_description, ''),
				coalesce(db.full_description, 'No full description'), coalesce(db.default_table, ''), db.public,
				coalesce(db.source_url, ''), coalesce(db.default_branch, ''), coalesce(db.live_node, ''),
				coalesce(db.live_minio_object_id, '')
			FROM sqlite_databases AS db
			WHERE db.user_id = (
					SELECT user_id
					FROM users
					WHERE lower(user_name) = lower($1)
				)
				AND db.db_name = $2
				AND db.is_deleted = false`

		// Retrieve the requested database details
		err = database.DB.QueryRow(context.Background(), dbQuery, dbOwner, dbName).Scan(&DB.Info.DateCreated,
			&DB.Info.RepoModified, &DB.Info.Watchers, &DB.Info.Stars, &DB.Info.Discussions, &DB.Info.OneLineDesc,
			&DB.Info.FullDesc, &DB.Info.DefaultTable, &DB.Info.Public, &DB.Info.SourceURL, &DB.Info.DefaultBranch,
			&DB.Info.LiveNode, &DB.MinioId)
		if err != nil {
			log.Printf("Error when retrieving database details: %v", err.Error())
			return errors.New("The requested database doesn't exist")
		}
		DB.Info.IsLive = true
	}

	// If an sha256 was in the licence field, retrieve its friendly name and url for displaying
	licSHA := DB.Info.DBEntry.LicenceSHA
	if licSHA != "" {
		DB.Info.Licence, DB.Info.LicenceURL, err = database.GetLicenceInfoFromSha256(dbOwner, licSHA)
		if err != nil {
			return err
		}
	} else {
		DB.Info.Licence = "Not specified"
	}

	// Retrieve correctly capitalised username for the database owner
	usrOwner, err := database.User(dbOwner)
	if err != nil {
		return err
	}

	// Fill out the fields we already have data for
	DB.Info.Database = dbName
	DB.Info.Owner = usrOwner.Username

	// The social stats are always updated because they could change without the cache being updated
	DB.Info.Watchers, DB.Info.Stars, DB.Info.Forks, err = SocialStats(dbOwner, dbName)
	if err != nil {
		return err
	}

	// Retrieve the latest discussion and MR counts
	DB.Info.Discussions, DB.Info.MRs, err = GetDiscussionAndMRCount(dbOwner, dbName)
	if err != nil {
		return err
	}

	// Retrieve the "forked from" information
	DB.Info.ForkOwner, DB.Info.ForkDatabase, DB.Info.ForkDeleted, err = ForkedFrom(dbOwner, dbName)
	if err != nil {
		return err
	}

	// Check if the database was starred by the logged in user
	DB.Info.MyStar, err = database.CheckDBStarred(loggedInUser, dbOwner, dbName)
	if err != nil {
		return err
	}

	// Check if the database is being watched by the logged in user
	DB.Info.MyWatch, err = database.CheckDBWatched(loggedInUser, dbOwner, dbName)
	if err != nil {
		return err
	}
	return nil
}

// DBStars returns the star count for a given database
func DBStars(dbOwner, dbName string) (starCount int, err error) {
	// Retrieve the updated star count
	dbQuery := `
		SELECT stars
		FROM sqlite_databases
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
			)
			AND db_name = $2
			AND is_deleted = false`
	err = database.DB.QueryRow(context.Background(), dbQuery, dbOwner, dbName).Scan(&starCount)
	if err != nil {
		log.Printf("Error looking up star count for database '%s/%s'. Error: %v", SanitiseLogString(dbOwner),
			SanitiseLogString(dbName), err)
		return -1, err
	}
	return starCount, nil
}

// DBWatchers returns the watchers count for a given database
func DBWatchers(dbOwner, dbName string) (watcherCount int, err error) {
	// Retrieve the updated watchers count
	dbQuery := `
		SELECT watchers
		FROM sqlite_databases
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
			)
			AND db_name = $2
			AND is_deleted = false`
	err = database.DB.QueryRow(context.Background(), dbQuery, dbOwner, dbName).Scan(&watcherCount)
	if err != nil {
		log.Printf("Error looking up watcher count for database '%s/%s'. Error: %v",
			SanitiseLogString(dbOwner), SanitiseLogString(dbName), err)
		return -1, err
	}
	return watcherCount, nil
}

// DefaultCommit returns the default commit ID for a specific database
func DefaultCommit(dbOwner, dbName string) (commitID string, err error) {
	// If no commit ID was supplied, we retrieve the latest commit ID from the default branch
	dbQuery := `
		SELECT branch_heads->default_branch->>'commit'::text AS commit_id
		FROM sqlite_databases
		WHERE user_id = (
				SELECT user_id
					FROM users
					WHERE lower(user_name) = lower($1)
			)
			AND db_name = $2
			AND is_deleted = false`
	var c pgtype.Text
	err = database.DB.QueryRow(context.Background(), dbQuery, dbOwner, dbName).Scan(&c)
	if err != nil {
		log.Printf("Error when retrieving head commit ID of default branch: %v", err.Error())
		return "", errors.New("Internal error when looking up database details")
	}
	if c.Valid {
		commitID = c.String
	}
	return commitID, nil
}

// DeleteDatabase deletes a database from PostgreSQL
// Note that we leave a stub/placeholder entry for all uploaded databases in PG, so our stats don't miss data over time
// and so the dependant table data doesn't go weird.  We also set the "is_deleted" boolean to true for its entry, so
// our database query functions know to skip it
func DeleteDatabase(dbOwner, dbName string) error {
	// Is this a live database
	isLive, _, err := CheckDBLive(dbOwner, dbName)
	if err != nil {
		return err
	}

	// Begin a transaction
	tx, err := database.DB.Begin(context.Background())
	if err != nil {
		return err
	}
	// Set up an automatic transaction roll back if the function exits without committing
	defer tx.Rollback(context.Background())

	// Remove all watchers for this database
	dbQuery := `
			DELETE FROM watchers
			WHERE db_id = (
					SELECT db_id
					FROM sqlite_databases
					WHERE user_id = (
							SELECT user_id
							FROM users
							WHERE lower(user_name) = lower($1)
						)
						AND db_name = $2
				)`
	commandTag, err := tx.Exec(context.Background(), dbQuery, dbOwner, dbName)
	if err != nil {
		log.Printf("Removing all watchers for database '%s/%s' failed: Error '%s'", SanitiseLogString(dbOwner),
			SanitiseLogString(dbName), err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("Wrong # of rows affected (%v) when removing all watchers for database '%s/%s'", numRows,
			SanitiseLogString(dbOwner), SanitiseLogString(dbName))
	}

	// Check if there are any forks of this database
	dbQuery = `
		WITH this_db AS (
			SELECT db_id
			FROM sqlite_databases
			WHERE user_id = (
					SELECT user_id
					FROM users
					WHERE lower(user_name) = lower($1)
				)
				AND db_name = $2
		)
		SELECT count(*)
		FROM sqlite_databases AS db, this_db
		WHERE db.forked_from = this_db.db_id`
	var numForks int
	err = tx.QueryRow(context.Background(), dbQuery, dbOwner, dbName).Scan(&numForks)
	if err != nil {
		log.Printf("Retrieving fork list failed for database '%s/%s': %s", SanitiseLogString(dbOwner),
			SanitiseLogString(dbName), err)
		return err
	}
	if numForks == 0 {
		// Update the fork count for the root database
		dbQuery = `
			WITH root_db AS (
				SELECT root_database AS id
				FROM sqlite_databases
				WHERE user_id = (
						SELECT user_id
						FROM users
						WHERE lower(user_name) = lower($1)
					)
					AND db_name = $2
			), new_count AS (
				SELECT count(*) AS forks
				FROM sqlite_databases AS db, root_db
				WHERE db.root_database = root_db.id
				AND db.is_deleted = false
			)
			UPDATE sqlite_databases
			SET forks = new_count.forks - 2
			FROM new_count, root_db
			WHERE sqlite_databases.db_id = root_db.id`
		commandTag, err := tx.Exec(context.Background(), dbQuery, dbOwner, dbName)
		if err != nil {
			log.Printf("Updating fork count for '%s/%s' in PostgreSQL failed: %s", SanitiseLogString(dbOwner),
				SanitiseLogString(dbName), err)
			return err
		}
		if numRows := commandTag.RowsAffected(); numRows != 1 && !isLive { // Skip this check when deleting live databases
			log.Printf("Wrong number of rows (%d) affected (spot 1) when updating fork count for database '%s/%s'",
				numRows, SanitiseLogString(dbOwner), SanitiseLogString(dbName))
		}

		// Generate a random string to be used in the deleted database's name field, so if the user adds a database with
		// the deleted one's name then the unique constraint on the database won't reject it
		newName := "deleted-database-" + RandomString(20)

		// Mark the database as deleted in PostgreSQL, replacing the entry with the ~randomly generated name
		dbQuery = `
			UPDATE sqlite_databases AS db
			SET is_deleted = true, public = false, db_name = $3, last_modified = now()
			WHERE user_id = (
					SELECT user_id
					FROM users
					WHERE lower(user_name) = lower($1)
				)
				AND db_name = $2`
		commandTag, err = tx.Exec(context.Background(), dbQuery, dbOwner, dbName, newName)
		if err != nil {
			log.Printf("%s: deleting (forked) database entry failed for database '%s/%s': %v",
				config.Conf.Live.Nodename, SanitiseLogString(dbOwner), SanitiseLogString(dbName), err)
			return err
		}
		if numRows := commandTag.RowsAffected(); numRows != 1 {
			log.Printf(
				"%s: wrong number of rows (%d) affected when deleting (forked) database '%s/%s'",
				config.Conf.Live.Nodename, numRows, SanitiseLogString(dbOwner), SanitiseLogString(dbName))
		}

		// Commit the transaction
		err = tx.Commit(context.Background())
		if err != nil {
			return err
		}

		// Log the database deletion
		log.Printf("%s: database '%s/%s' deleted", config.Conf.Live.Nodename, SanitiseLogString(dbOwner), SanitiseLogString(dbName))
		return nil
	}

	// Delete all stars referencing the database stub
	dbQuery = `
		DELETE FROM database_stars
		WHERE db_id = (
			SELECT db_id
			FROM sqlite_databases
			WHERE user_id = (
					SELECT user_id
					FROM users
					WHERE lower(user_name) = lower($1)
				)
				AND db_name = $2
			)`
	commandTag, err = tx.Exec(context.Background(), dbQuery, dbOwner, dbName)
	if err != nil {
		log.Printf("Deleting (forked) database stars failed for database '%s/%s': %v",
			SanitiseLogString(dbOwner), SanitiseLogString(dbName), err)
		return err
	}

	// Generate a random string to be used in the deleted database's name field, so if the user adds a database with
	// the deleted one's name then the unique constraint on the database won't reject it
	newName := "deleted-database-" + RandomString(20)

	// Replace the database entry in sqlite_databases with a stub
	dbQuery = `
		UPDATE sqlite_databases AS db
		SET is_deleted = true, public = false, db_name = $3, last_modified = now()
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
			)
			AND db_name = $2`
	commandTag, err = tx.Exec(context.Background(), dbQuery, dbOwner, dbName, newName)
	if err != nil {
		log.Printf("Deleting (forked) database entry failed for database '%s/%s': %v",
			SanitiseLogString(dbOwner), SanitiseLogString(dbName), err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf(
			"Wrong number of rows (%d) affected when deleting (forked) database '%s/%s'", numRows,
			SanitiseLogString(dbOwner), SanitiseLogString(dbName))
	}

	// Update the fork count for the root database
	dbQuery = `
		WITH root_db AS (
			SELECT root_database AS id
			FROM sqlite_databases
			WHERE user_id = (
					SELECT user_id
					FROM users
					WHERE lower(user_name) = lower($1)
				)
				AND db_name = $2
		), new_count AS (
			SELECT count(*) AS forks
			FROM sqlite_databases AS db, root_db
			WHERE db.root_database = root_db.id
			AND db.is_deleted = false
		)
		UPDATE sqlite_databases
		SET forks = new_count.forks - 1
		FROM new_count, root_db
		WHERE sqlite_databases.db_id = root_db.id`
	commandTag, err = tx.Exec(context.Background(), dbQuery, dbOwner, newName)
	if err != nil {
		log.Printf("Updating fork count for '%s/%s' in PostgreSQL failed: %v", SanitiseLogString(dbOwner),
			SanitiseLogString(dbName), err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("Wrong number of rows (%d) affected (spot 2) when updating fork count for database '%s/%s'",
			numRows, SanitiseLogString(dbOwner), SanitiseLogString(dbName))
	}

	// Commit the transaction
	err = tx.Commit(context.Background())
	if err != nil {
		return err
	}

	// Log the database deletion
	log.Printf("%s: (forked) database '%s/%s' deleted", config.Conf.Live.Nodename, SanitiseLogString(dbOwner),
		SanitiseLogString(dbName))
	return nil
}

// FlushViewCount periodically flushes the database view count from Memcache to PostgreSQL
func FlushViewCount() {
	type dbEntry struct {
		Owner string
		Name  string
	}

	// Log the start of the loop
	log.Printf("%s: periodic view count flushing loop started.  %d second refresh.", config.Conf.Live.Nodename, config.Conf.Memcache.ViewCountFlushDelay)

	// Start the endless flush loop
	var rows pgx.Rows
	var err error
	for {
		// Retrieve the list of all public databases
		dbQuery := `
			SELECT users.user_name, db.db_name
			FROM sqlite_databases AS db, users
			WHERE db.public = true
				AND db.is_deleted = false
				AND db.user_id = users.user_id`
		rows, err = database.DB.Query(context.Background(), dbQuery)
		if err != nil {
			log.Printf("Database query failed: %v", err)
			continue
		}
		var dbList []dbEntry
		for rows.Next() {
			var oneRow dbEntry
			err = rows.Scan(&oneRow.Owner, &oneRow.Name)
			if err != nil {
				log.Printf("Error retrieving database list for view count flush thread: %v", err)
				rows.Close()
				continue
			}
			dbList = append(dbList, oneRow)
		}
		rows.Close()

		// For each public database, retrieve the latest view count from memcache and save it back to PostgreSQL
		for _, db := range dbList {
			dbOwner := db.Owner
			dbName := db.Name

			// Retrieve the view count from Memcached
			newValue, err := GetViewCount(dbOwner, dbName)
			if err != nil {
				log.Printf("Error when getting memcached view count for %s/%s: %s", dbOwner, dbName,
					err.Error())
				continue
			}

			// We use a value of -1 to indicate there wasn't an entry in memcache for the database
			if newValue != -1 {
				// Update the view count in PostgreSQL
				dbQuery = `
					UPDATE sqlite_databases
					SET page_views = $3
					WHERE user_id = (
							SELECT user_id
							FROM users
							WHERE lower(user_name) = lower($1)
						)
						AND db_name = $2`
				commandTag, err := database.DB.Exec(context.Background(), dbQuery, dbOwner, dbName, newValue)
				if err != nil {
					log.Printf("Flushing view count for '%s/%s' failed: %v", dbOwner, dbName, err)
					continue
				}
				if numRows := commandTag.RowsAffected(); numRows != 1 {
					log.Printf("Wrong number of rows affected (%v) when flushing view count for '%s/%s'",
						numRows, dbOwner, dbName)
					continue
				}
			}
		}

		// Wait before running the loop again
		time.Sleep(config.Conf.Memcache.ViewCountFlushDelay * time.Second)
	}

	// If somehow the endless loop finishes, then record that in the server logs
	log.Printf("%s: WARN: periodic view count flushing loop stopped.", config.Conf.Live.Nodename)
}

// ForkDatabase forks the PostgreSQL entry for a SQLite database from one user to another
func ForkDatabase(srcOwner, dbName, dstOwner string) (newForkCount int, err error) {
	// Copy the main database entry
	dbQuery := `
		WITH dst_u AS (
			SELECT user_id
			FROM users
			WHERE lower(user_name) = lower($1)
		)
		INSERT INTO sqlite_databases (user_id, db_name, public, forks, one_line_description, full_description,
			branches, contributors, root_database, default_table, source_url, commit_list, branch_heads, tags,
			default_branch, forked_from)
		SELECT dst_u.user_id, db_name, public, 0, one_line_description, full_description, branches,
			contributors, root_database, default_table, source_url, commit_list, branch_heads, tags, default_branch,
			db_id
		FROM sqlite_databases, dst_u
		WHERE sqlite_databases.user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($2)
			)
			AND db_name = $3`
	commandTag, err := database.DB.Exec(context.Background(), dbQuery, dstOwner, srcOwner, dbName)
	if err != nil {
		log.Printf("Forking database '%s/%s' in PostgreSQL failed: %v", SanitiseLogString(srcOwner),
			SanitiseLogString(dbName), err)
		return 0, err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("Wrong number of rows affected (%d) when forking main database entry: "+
			"'%s/%s' to '%s/%s'", numRows, SanitiseLogString(srcOwner), SanitiseLogString(dbName),
			dstOwner, SanitiseLogString(dbName))
	}

	// Update the fork count for the root database
	dbQuery = `
		WITH root_db AS (
			SELECT root_database AS id
			FROM sqlite_databases
			WHERE user_id = (
					SELECT user_id
					FROM users
					WHERE lower(user_name) = lower($1)
				)
				AND db_name = $2
		), new_count AS (
			SELECT count(*) AS forks
			FROM sqlite_databases AS db, root_db
			WHERE db.root_database = root_db.id
			AND db.is_deleted = false
		)
		UPDATE sqlite_databases
		SET forks = new_count.forks - 1
		FROM new_count, root_db
		WHERE sqlite_databases.db_id = root_db.id
		RETURNING new_count.forks - 1`
	err = database.DB.QueryRow(context.Background(), dbQuery, dstOwner, dbName).Scan(&newForkCount)
	if err != nil {
		log.Printf("Updating fork count in PostgreSQL failed: %v", err)
		return 0, err
	}
	return newForkCount, nil
}

// ForkedFrom checks if the given database was forked from another, and if so returns that one's owner and
// database name
func ForkedFrom(dbOwner, dbName string) (forkOwn, forkDB string, forkDel bool, err error) {
	// Check if the database was forked from another
	var dbID, forkedFrom pgtype.Int8
	dbQuery := `
		SELECT db_id, forked_from
		FROM sqlite_databases
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1))
			AND db_name = $2`
	err = database.DB.QueryRow(context.Background(), dbQuery, dbOwner, dbName).Scan(&dbID, &forkedFrom)
	if err != nil {
		log.Printf("Error checking if database was forked from another '%s/%s'. Error: %v",
			SanitiseLogString(dbOwner), SanitiseLogString(dbName), err)
		return "", "", false, err
	}
	if !forkedFrom.Valid {
		// The database wasn't forked, so return empty strings
		return "", "", false, nil
	}

	// Return the details of the database this one was forked from
	dbQuery = `
		SELECT u.user_name, db.db_name, db.is_deleted
		FROM users AS u, sqlite_databases AS db
		WHERE db.db_id = $1
			AND u.user_id = db.user_id`
	err = database.DB.QueryRow(context.Background(), dbQuery, forkedFrom).Scan(&forkOwn, &forkDB, &forkDel)
	if err != nil {
		log.Printf("Error retrieving forked database information for '%s/%s'. Error: %v",
			SanitiseLogString(dbOwner), SanitiseLogString(dbName), err)
		return "", "", false, err
	}

	// If the database this one was forked from has been deleted, indicate that and clear the database name value
	if forkDel {
		forkDB = ""
	}
	return forkOwn, forkDB, forkDel, nil
}

// ForkParent returns the parent of a database, if there is one (and it's accessible to the logged in user).  If no
// parent was found, the returned Owner/DBName values will be empty strings
func ForkParent(loggedInUser, dbOwner, dbName string) (parentOwner, parentDBName string, err error) {
	dbQuery := `
		SELECT users.user_name, db.db_name, db.public, db.db_id, db.forked_from, db.is_deleted
		FROM sqlite_databases AS db, users
		WHERE db.root_database = (
				SELECT root_database
				FROM sqlite_databases
				WHERE user_id = (
						SELECT user_id
						FROM users
						WHERE lower(user_name) = lower($1)
					)
					AND db_name = $2
				)
			AND db.user_id = users.user_id
		ORDER BY db.forked_from NULLS FIRST`
	rows, err := database.DB.Query(context.Background(), dbQuery, dbOwner, dbName)
	if err != nil {
		log.Printf("Database query failed: %v", err)
		return
	}
	defer rows.Close()
	dbList := make(map[int]ForkEntry)
	for rows.Next() {
		var frk pgtype.Int8
		var oneRow ForkEntry
		err = rows.Scan(&oneRow.Owner, &oneRow.DBName, &oneRow.Public, &oneRow.ID, &frk, &oneRow.Deleted)
		if err != nil {
			log.Printf("Error retrieving fork parent for '%s/%s': %v", dbOwner, dbName,
				err)
			return
		}
		if frk.Valid {
			oneRow.ForkedFrom = int(frk.Int64)
		}
		dbList[oneRow.ID] = oneRow
	}

	// Safety check
	numResults := len(dbList)
	if numResults == 0 {
		err = fmt.Errorf("Empty list returned instead of fork tree.  This shouldn't happen.")
		return
	}

	// Get the ID of the database being called
	dbID, err := databaseID(dbOwner, dbName)
	if err != nil {
		return
	}

	// Find the closest (not-deleted) parent for the database
	dbEntry, ok := dbList[dbID]
	if !ok {
		// The database itself wasn't found in the list.  This shouldn't happen
		err = fmt.Errorf("Internal error when retrieving fork parent info.  This shouldn't happen.")
		return
	}
	for dbEntry.ForkedFrom != 0 {
		dbEntry, ok = dbList[dbEntry.ForkedFrom]
		if !ok {
			// Parent database entry wasn't found in the list.  This shouldn't happen either
			err = fmt.Errorf("Internal error when retrieving fork parent info (#2).  This shouldn't happen.")
			return
		}
		if !dbEntry.Deleted {
			// Found a parent (that's not deleted).  We'll use this and stop looping
			parentOwner = dbEntry.Owner
			parentDBName = dbEntry.DBName
			break
		}
	}
	return
}

// ForkTree returns the complete fork tree for a given database
func ForkTree(loggedInUser, dbOwner, dbName string) (outputList []ForkEntry, err error) {
	dbQuery := `
		SELECT users.user_name, db.db_name, db.public, db.db_id, db.forked_from, db.is_deleted
		FROM sqlite_databases AS db, users
		WHERE db.root_database = (
				SELECT root_database
				FROM sqlite_databases
				WHERE user_id = (
						SELECT user_id
						FROM users
						WHERE lower(user_name) = lower($1)
					)
					AND db_name = $2
				)
			AND db.user_id = users.user_id
		ORDER BY db.forked_from NULLS FIRST`
	rows, err := database.DB.Query(context.Background(), dbQuery, dbOwner, dbName)
	if err != nil {
		log.Printf("Database query failed: %v", err)
		return nil, err
	}
	defer rows.Close()
	var dbList []ForkEntry
	for rows.Next() {
		var frk pgtype.Int8
		var oneRow ForkEntry
		err = rows.Scan(&oneRow.Owner, &oneRow.DBName, &oneRow.Public, &oneRow.ID, &frk, &oneRow.Deleted)
		if err != nil {
			log.Printf("Error retrieving fork list for '%s/%s': %v", dbOwner, dbName, err)
			return nil, err
		}
		if frk.Valid {
			oneRow.ForkedFrom = int(frk.Int64)
		}
		dbList = append(dbList, oneRow)
	}

	// Safety checks
	numResults := len(dbList)
	if numResults == 0 {
		return nil, errors.New("Empty list returned instead of fork tree.  This shouldn't happen.")
	}
	if dbList[0].ForkedFrom != 0 {
		// The first entry has a non-zero forked_from field, indicating it's not the root entry.  That
		// shouldn't happen, so return an error.
		return nil, errors.New("Incorrect root entry data in retrieved database list.")
	}

	// * Process the root entry *

	var iconDepth int
	var forkTrail []int

	// Set the root database ID
	rootID := dbList[0].ID

	// Set the icon list for display in the browser
	dbList[0].IconList = append(dbList[0].IconList, ROOT)

	// If the root database is no longer public, then use placeholder details instead
	if !dbList[0].Public && (strings.ToLower(dbList[0].Owner) != strings.ToLower(loggedInUser)) {
		dbList[0].DBName = "private database"
	}

	// If the root database is deleted, use a placeholder indicating that instead
	if dbList[0].Deleted {
		dbList[0].DBName = "deleted database"
	}

	// Append this completed database line to the output list
	outputList = append(outputList, dbList[0])

	// Append the root database ID to the fork trail
	forkTrail = append(forkTrail, rootID)

	// Mark the root database entry as processed
	dbList[0].Processed = true

	// Increment the icon depth
	iconDepth = 1

	// * Sort the remaining entries for correct display *
	numUnprocessedEntries := numResults - 1
	for numUnprocessedEntries > 0 {
		var forkFound bool
		outputList, forkTrail, forkFound = nextChild(loggedInUser, &dbList, &outputList, &forkTrail, iconDepth)
		if forkFound {
			numUnprocessedEntries--
			iconDepth++

			// Add stems and branches to the output icon list
			numOutput := len(outputList)

			myID := outputList[numOutput-1].ID
			myForkedFrom := outputList[numOutput-1].ForkedFrom

			// Scan through the earlier output list for any sibling entries
			var siblingFound bool
			for i := numOutput; i > 0 && siblingFound == false; i-- {
				thisID := outputList[i-1].ID
				thisForkedFrom := outputList[i-1].ForkedFrom

				if thisForkedFrom == myForkedFrom && thisID != myID {
					// Sibling entry found
					siblingFound = true
					sibling := outputList[i-1]

					// Change the last sibling icon to a branch icon
					sibling.IconList[iconDepth-1] = BRANCH

					// Change appropriate spaces to stems in the output icon list
					for l := numOutput - 1; l > i; l-- {
						thisEntry := outputList[l-1]
						if thisEntry.IconList[iconDepth-1] == SPACE {
							thisEntry.IconList[iconDepth-1] = STEM
						}
					}
				}
			}
		} else {
			// No child was found, so remove an entry from the fork trail then continue looping
			forkTrail = forkTrail[:len(forkTrail)-1]

			iconDepth--
		}
	}

	return outputList, nil
}

// GetActivityStats returns the latest activity stats
func GetActivityStats() (stats ActivityStats, err error) {
	// Retrieve a list of which databases are the most starred
	dbQuery := `
		WITH most_starred AS (
			SELECT s.db_id, COUNT(s.db_id), max(s.date_starred)
			FROM database_stars AS s, sqlite_databases AS db
			WHERE s.db_id = db.db_id
				AND db.public = true
				AND db.is_deleted = false
			GROUP BY s.db_id
			ORDER BY count DESC
			LIMIT 5
		)
		SELECT users.user_name, db.db_name, stars.count
		FROM most_starred AS stars, sqlite_databases AS db, users
		WHERE stars.db_id = db.db_id
			AND users.user_id = db.user_id
		ORDER BY count DESC, max ASC`
	starRows, err := database.DB.Query(context.Background(), dbQuery)
	if err != nil {
		log.Printf("Database query failed: %v", err)
		return
	}
	defer starRows.Close()
	for starRows.Next() {
		var oneRow ActivityRow
		err = starRows.Scan(&oneRow.Owner, &oneRow.DBName, &oneRow.Count)
		if err != nil {
			log.Printf("Error retrieving list of most starred databases: %v", err)
			return
		}
		stats.Starred = append(stats.Starred, oneRow)
	}

	// Retrieve a list of which databases are the most forked
	dbQuery = `
		SELECT users.user_name, db.db_name, db.forks
		FROM sqlite_databases AS db, users
		WHERE db.forks > 0
			AND db.public = true
			AND db.is_deleted = false
			AND db.user_id = users.user_id
		ORDER BY db.forks DESC, db.last_modified
		LIMIT 5`
	forkRows, err := database.DB.Query(context.Background(), dbQuery)
	if err != nil {
		log.Printf("Database query failed: %v", err)
		return
	}
	defer forkRows.Close()
	for forkRows.Next() {
		var oneRow ActivityRow
		err = forkRows.Scan(&oneRow.Owner, &oneRow.DBName, &oneRow.Count)
		if err != nil {
			log.Printf("Error retrieving list of most forked databases: %v", err)
			return
		}
		stats.Forked = append(stats.Forked, oneRow)
	}

	// Retrieve a list of the most recent uploads
	dbQuery = `
		SELECT user_name, db.db_name, db.last_modified
		FROM sqlite_databases AS db, users
		WHERE db.forked_from IS NULL
			AND db.public = true
			AND db.is_deleted = false
			AND db.user_id = users.user_id
		ORDER BY db.last_modified DESC
		LIMIT 5`
	upRows, err := database.DB.Query(context.Background(), dbQuery)
	if err != nil {
		log.Printf("Database query failed: %v", err)
		return
	}
	defer upRows.Close()
	for upRows.Next() {
		var oneRow UploadRow
		err = upRows.Scan(&oneRow.Owner, &oneRow.DBName, &oneRow.UploadDate)
		if err != nil {
			log.Printf("Error retrieving list of most recent uploads: %v", err)
			return
		}
		stats.Uploads = append(stats.Uploads, oneRow)
	}

	// Retrieve a list of which databases have been downloaded the most times by someone other than their owner
	dbQuery = `
		SELECT users.user_name, db.db_name, db.download_count
		FROM sqlite_databases AS db, users
		WHERE db.download_count > 0
			AND db.public = true
			AND db.is_deleted = false
			AND db.user_id = users.user_id
		ORDER BY db.download_count DESC, db.last_modified
		LIMIT 5`
	dlRows, err := database.DB.Query(context.Background(), dbQuery)
	if err != nil {
		log.Printf("Database query failed: %v", err)
		return
	}
	defer dlRows.Close()
	for dlRows.Next() {
		var oneRow ActivityRow
		err = dlRows.Scan(&oneRow.Owner, &oneRow.DBName, &oneRow.Count)
		if err != nil {
			log.Printf("Error retrieving list of most downloaded databases: %v", err)
			return
		}
		stats.Downloads = append(stats.Downloads, oneRow)
	}

	// Retrieve the list of databases which have been viewed the most times
	dbQuery = `
		SELECT users.user_name, db.db_name, db.page_views
		FROM sqlite_databases AS db, users
		WHERE db.page_views > 0
			AND db.public = true
			AND db.is_deleted = false
			AND db.user_id = users.user_id
		ORDER BY db.page_views DESC, db.last_modified
		LIMIT 5`
	viewRows, err := database.DB.Query(context.Background(), dbQuery)
	if err != nil {
		log.Printf("Database query failed: %v", err)
		return
	}
	defer viewRows.Close()
	for viewRows.Next() {
		var oneRow ActivityRow
		err = viewRows.Scan(&oneRow.Owner, &oneRow.DBName, &oneRow.Count)
		if err != nil {
			log.Printf("Error retrieving list of most viewed databases: %v", err)
			return
		}
		stats.Viewed = append(stats.Viewed, oneRow)
	}
	return
}

// GetBranches load the branch heads for a database
// TODO: It might be better to have the default branch name be returned as part of this list, by indicating in the list
// TODO  which of the branches is the default.
func GetBranches(dbOwner, dbName string) (branches map[string]BranchEntry, err error) {
	dbQuery := `
		SELECT db.branch_heads
		FROM sqlite_databases AS db
		WHERE db.user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
			)
			AND db.db_name = $2`
	err = database.DB.QueryRow(context.Background(), dbQuery, dbOwner, dbName).Scan(&branches)
	if err != nil {
		log.Printf("Error when retrieving branch heads for database '%s/%s': %v", SanitiseLogString(dbOwner),
			SanitiseLogString(dbName), err)
		return nil, err
	}
	return branches, nil
}

// GetCommitList returns the full commit list for a database
func GetCommitList(dbOwner, dbName string) (map[string]database.CommitEntry, error) {
	dbQuery := `
		WITH u AS (
			SELECT user_id
			FROM users
			WHERE lower(user_name) = lower($1)
		)
		SELECT commit_list as commits
		FROM sqlite_databases AS db, u
		WHERE db.user_id = u.user_id
			AND db.db_name = $2
			AND db.is_deleted = false`
	var l map[string]database.CommitEntry
	err := database.DB.QueryRow(context.Background(), dbQuery, dbOwner, dbName).Scan(&l)
	if err != nil {
		log.Printf("Retrieving commit list for '%s/%s' failed: %v", SanitiseLogString(dbOwner),
			SanitiseLogString(dbName), err)
		return map[string]database.CommitEntry{}, err
	}
	return l, nil
}

// GetDefaultBranchName returns the default branch name for a database
func GetDefaultBranchName(dbOwner, dbName string) (branchName string, err error) {
	dbQuery := `
		SELECT db.default_branch
		FROM sqlite_databases AS db
		WHERE db.user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
			)
			AND db.db_name = $2
			AND db.is_deleted = false`
	var b pgtype.Text
	err = database.DB.QueryRow(context.Background(), dbQuery, dbOwner, dbName).Scan(&b)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("Error when retrieving default branch name for database '%s/%s': %v",
				SanitiseLogString(dbOwner), SanitiseLogString(dbName), err)
		} else {
			log.Printf("No default branch name exists for database '%s/%s'. This shouldn't happen",
				SanitiseLogString(dbOwner), SanitiseLogString(dbName))
		}
		return
	}
	if b.Valid {
		branchName = b.String
	}
	return
}

// GetDefaultTableName returns the default table name for a database
func GetDefaultTableName(dbOwner, dbName string) (tableName string, err error) {
	dbQuery := `
		SELECT db.default_table
		FROM sqlite_databases AS db
		WHERE db.user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
			)
			AND db.db_name = $2
			AND db.is_deleted = false`
	var t pgtype.Text
	err = database.DB.QueryRow(context.Background(), dbQuery, dbOwner, dbName).Scan(&t)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("Error when retrieving default table name for database '%s/%s': %v",
				SanitiseLogString(dbOwner), SanitiseLogString(dbName), err)
			return
		}
	}
	if t.Valid {
		tableName = t.String
	}
	return
}

// GetDiscussionAndMRCount returns the discussion and merge request counts for a database
// TODO: The only reason this function exists atm, is because we're incorrectly caching the discussion and MR data in
// TODO  a way that makes invalidating it correctly hard/impossible.  We should redo our memcached approach to solve the
// TODO  issue properly
func GetDiscussionAndMRCount(dbOwner, dbName string) (discCount, mrCount int, err error) {
	dbQuery := `
		SELECT db.discussions, db.merge_requests
		FROM sqlite_databases AS db
		WHERE db.user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
			)
			AND db.db_name = $2
			AND db.is_deleted = false`
	err = database.DB.QueryRow(context.Background(), dbQuery, dbOwner, dbName).Scan(&discCount, &mrCount)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("Error when retrieving discussion and MR count for database '%s/%s': %v",
				SanitiseLogString(dbOwner), SanitiseLogString(dbName), err)
		} else {
			log.Printf("Database '%s/%s' not found when attempting to retrieve discussion and MR count. This"+
				"shouldn't happen", SanitiseLogString(dbOwner), SanitiseLogString(dbName))
		}
		return
	}
	return
}

// GetReleases returns the list of releases for a database
func GetReleases(dbOwner, dbName string) (releases map[string]ReleaseEntry, err error) {
	dbQuery := `
		SELECT release_list
		FROM sqlite_databases
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
			)
			AND db_name = $2`
	err = database.DB.QueryRow(context.Background(), dbQuery, dbOwner, dbName).Scan(&releases)
	if err != nil {
		log.Printf("Error when retrieving releases for database '%s/%s': %v", SanitiseLogString(dbOwner),
			SanitiseLogString(dbName), err)
		return nil, err
	}
	if releases == nil {
		// If there aren't any releases yet, return an empty set instead of nil
		releases = make(map[string]ReleaseEntry)
	}
	return releases, nil
}

// GetTags returns the tags for a database
func GetTags(dbOwner, dbName string) (tags map[string]TagEntry, err error) {
	dbQuery := `
		SELECT tag_list
		FROM sqlite_databases
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
			)
			AND db_name = $2`
	err = database.DB.QueryRow(context.Background(), dbQuery, dbOwner, dbName).Scan(&tags)
	if err != nil {
		log.Printf("Error when retrieving tags for database '%s/%s': %v", SanitiseLogString(dbOwner),
			SanitiseLogString(dbName), err)
		return nil, err
	}
	if tags == nil {
		// If there aren't any tags yet, return an empty set instead of nil
		tags = make(map[string]TagEntry)
	}
	return tags, nil
}

// IncrementDownloadCount increments the download count for a database
func IncrementDownloadCount(dbOwner, dbName string) error {
	dbQuery := `
		UPDATE sqlite_databases
		SET download_count = download_count + 1
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
			)
			AND db_name = $2`
	commandTag, err := database.DB.Exec(context.Background(), dbQuery, dbOwner, dbName)
	if err != nil {
		log.Printf("Increment download count for '%s/%s' failed: %v", SanitiseLogString(dbOwner),
			SanitiseLogString(dbName), err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		errMsg := fmt.Sprintf("Wrong number of rows affected (%v) when incrementing download count for '%s/%s'",
			numRows, dbOwner, dbName)
		log.Printf(SanitiseLogString(errMsg))
		return errors.New(errMsg)
	}
	return nil
}

// LiveAddDatabasePG adds the details for a live database to PostgreSQL
func LiveAddDatabasePG(dbOwner, dbName, bucketName, liveNode string, accessType SetAccessType) (err error) {
	// Figure out new public/private access setting
	var public bool
	switch accessType {
	case SetToPublic:
		public = true
	case SetToPrivate:
		public = false
	default:
		err = errors.New("Error: Unknown public/private setting requested for a new live database.  Aborting.")
		return
	}

	var commandTag pgconn.CommandTag
	dbQuery := `
		WITH root AS (
			SELECT nextval('sqlite_databases_db_id_seq') AS val
		)
		INSERT INTO sqlite_databases (user_id, db_id, db_name, public, live_db, live_node, live_minio_object_id)
		SELECT (
			SELECT user_id
			FROM users
			WHERE lower(user_name) = lower($1)), (SELECT val FROM root), $2, $3, true, $4, $5`
	commandTag, err = database.DB.Exec(context.Background(), dbQuery, dbOwner, dbName, public, liveNode, bucketName)
	if err != nil {
		log.Printf("Storing LIVE database '%s/%s' failed: %s", SanitiseLogString(dbOwner), SanitiseLogString(dbName), err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("Wrong number of rows (%d) affected while storing LIVE database '%s/%s'", numRows,
			SanitiseLogString(dbOwner), SanitiseLogString(dbName))
	}
	return nil
}

// LiveGenerateMinioNames generates Minio bucket and object names for a live database
func LiveGenerateMinioNames(userName string) (bucketName, objectName string, err error) {
	// If the user already has a Minio bucket name assigned, then we use it
	z, err := database.User(userName)
	if err != nil {
		return
	}
	if z.MinioBucket != "" {
		bucketName = z.MinioBucket
	} else {
		// They don't have a bucket name assigned yet, so we generate one and assign it to them
		bucketName = fmt.Sprintf("live-%s", RandomString(10))

		// Add this bucket name to the user's details in the PG backend
		dbQuery := `
			UPDATE users
			SET live_minio_bucket_name = $2
			WHERE user_name = $1
			AND live_minio_bucket_name is null` // This should ensure we never overwrite an existing bucket name for the user
		var commandTag pgconn.CommandTag
		commandTag, err = database.DB.Exec(context.Background(), dbQuery, userName, bucketName)
		if err != nil {
			log.Printf("Updating Minio bucket name for user '%s' failed: %v", userName, err)
			return
		}
		if numRows := commandTag.RowsAffected(); numRows != 1 {
			log.Printf("Wrong number of rows (%d) affected while updating the Minio bucket name for user '%s'",
				numRows, userName)
		}
	}

	// We only generate the name here, we *do not* try to update anything in the database with it.  This is because
	// when this function is called, the SQLite database may not yet have a record in the PG backend
	objectName = RandomString(6)
	return
}

// LiveGetMinioNames retrieves the Minio bucket and object names for a live database
func LiveGetMinioNames(loggedInUser, dbOwner, dbName string) (bucketName, objectName string, err error) {
	// Retrieve user details
	usr, err := database.User(dbOwner)
	if err != nil {
		return
	}

	// Retrieve database details
	var db SQLiteDBinfo
	err = DBDetails(&db, loggedInUser, dbOwner, dbName, "")
	if err != nil {
		return
	}

	// If either the user bucket name or the minio object name is empty, then the database is likely stored using
	// the initial naming scheme
	if usr.MinioBucket == "" || db.MinioId == "" {
		bucketName = fmt.Sprintf("live-%s", dbOwner)
		objectName = dbName
	} else {
		// It's using the new naming scheme
		bucketName = usr.MinioBucket
		objectName = db.MinioId
	}
	return
}

// LiveUserDBs returns the list of live databases owned by the user
func LiveUserDBs(dbOwner string, public AccessType) (list []DBInfo, err error) {
	dbQuery := `
		SELECT db_name, date_created, last_modified, public, live_db, live_node,
			db.watchers, db.stars, discussions, contributors,
			coalesce(one_line_description, ''), coalesce(source_url, ''),
			download_count, page_views
		FROM sqlite_databases AS db, users
		WHERE users.user_id = db.user_id
			AND lower(users.user_name) = lower($1)
			AND is_deleted = false
			AND live_db = true`

	switch public {
	case DB_PUBLIC:
		// Only public databases
		dbQuery += ` AND public = true`
	case DB_PRIVATE:
		// Only private databases
		dbQuery += ` AND public = false`
	case DB_BOTH:
		// Both public and private, so no need to add a query clause
	default:
		// This clause shouldn't ever be reached
		return nil, fmt.Errorf("Incorrect 'public' value '%v' passed to LiveUserDBs() function.", public)
	}
	dbQuery += " ORDER BY date_created DESC"

	rows, err := database.DB.Query(context.Background(), dbQuery, dbOwner)
	if err != nil {
		log.Printf("Database query failed: %v", err)
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var oneRow DBInfo
		var liveNode string
		err = rows.Scan(&oneRow.Database, &oneRow.DateCreated, &oneRow.RepoModified, &oneRow.Public, &oneRow.IsLive, &liveNode,
			&oneRow.Watchers, &oneRow.Stars, &oneRow.Discussions, &oneRow.Contributors,
			&oneRow.OneLineDesc, &oneRow.SourceURL, &oneRow.Downloads, &oneRow.Views)
		if err != nil {
			log.Printf("Error when retrieving list of live databases for user '%s': %v", dbOwner, err)
			return nil, err
		}

		// Ask the job queue backend for the database file size
		oneRow.Size, err = LiveSize(liveNode, dbOwner, dbOwner, oneRow.Database)
		if err != nil {
			log.Printf("Error when retrieving size of live databases for user '%s': %v", dbOwner, err)
			return nil, err
		}

		list = append(list, oneRow)
	}
	return
}

// MinioLocation returns the Minio bucket and ID for a given database. dbOwner & dbName are from
// owner/database URL fragment, loggedInUser is the name for the currently logged in user, for access permission
// check.  Use an empty string ("") as the loggedInUser parameter if the true value isn't set or known.
// If the requested database doesn't exist, or the loggedInUser doesn't have access to it, then an error will be
// returned
func MinioLocation(dbOwner, dbName, commitID, loggedInUser string) (minioBucket, minioID string, lastModified time.Time, err error) {
	// Check permissions
	allowed, err := database.CheckDBPermissions(loggedInUser, dbOwner, dbName, false)
	if err != nil {
		return
	}
	if !allowed {
		err = errors.New("Database not found")
		return
	}

	// If no commit was provided, we grab the default one
	if commitID == "" {
		commitID, err = DefaultCommit(dbOwner, dbName)
		if err != nil {
			return // Bucket and ID are still the initial default empty string
		}
	}

	// Retrieve the sha256 and last modified date for the requested commits database file
	var dbQuery string
	dbQuery = `
		SELECT commit_list->$3::text->'tree'->'entries'->0->>'sha256' AS sha256,
			commit_list->$3::text->'tree'->'entries'->0->>'last_modified' AS last_modified
		FROM sqlite_databases AS db
		WHERE db.user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
			)
			AND db.db_name = $2
			AND db.is_deleted = false`
	var sha, mod pgtype.Text
	err = database.DB.QueryRow(context.Background(), dbQuery, dbOwner, dbName, commitID).Scan(&sha, &mod)
	if err != nil {
		log.Printf("Error retrieving MinioID for '%s/%s' version '%v' by logged in user '%v': %v",
			SanitiseLogString(dbOwner), SanitiseLogString(dbName), SanitiseLogString(commitID), loggedInUser, err)
		return // Bucket and ID are still the initial default empty string
	}

	if !sha.Valid || sha.String == "" {
		// The requested database doesn't exist, or the logged in user doesn't have access to it
		err = fmt.Errorf("The requested database wasn't found")
		return // Bucket and ID are still the initial default empty string
	}

	lastModified, err = time.Parse(time.RFC3339, mod.String)
	if err != nil {
		return // Bucket and ID are still the initial default empty string
	}

	shaStr := sha.String
	minioBucket = shaStr[:MinioFolderChars]
	minioID = shaStr[MinioFolderChars:]
	return
}

// RenameDatabase renames a SQLite database
func RenameDatabase(userName, dbName, newName string) error {
	// Save the database settings
	dbQuery := `
		UPDATE sqlite_databases
		SET db_name = $3
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
			)
			AND db_name = $2`
	commandTag, err := database.DB.Exec(context.Background(), dbQuery, userName, dbName, newName)
	if err != nil {
		log.Printf("Renaming database '%s/%s' failed: %v", SanitiseLogString(userName),
			SanitiseLogString(dbName), err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		errMsg := fmt.Sprintf("Wrong number of rows affected (%d) when renaming '%s/%s' to '%s/%s'",
			numRows, userName, dbName, userName, newName)
		log.Printf(SanitiseLogString(errMsg))
		return errors.New(errMsg)
	}

	// Log the rename
	log.Printf("Database renamed from '%s/%s' to '%s/%s'", SanitiseLogString(userName), SanitiseLogString(dbName),
		SanitiseLogString(userName), SanitiseLogString(newName))
	return nil
}

// ResetDB resets the database to its default state. eg for testing purposes
func ResetDB() error {
	// We probably don't want to drop the database itself, as that'd screw up the current database
	// connection.  Instead, lets truncate all the tables then load their default values
	tableNames := []string{
		"api_call_log",
		"api_keys",
		"database_downloads",
		"database_licences",
		"database_shares",
		"database_stars",
		"database_uploads",
		"db4s_connects",
		"discussion_comments",
		"discussions",
		"email_queue",
		"events",
		"sql_terminal_history",
		"sqlite_databases",
		"users",
		"vis_params",
		"vis_query_runs",
		"watchers",
	}

	sequenceNames := []string{
		"api_keys_key_id_seq",
		"api_log_log_id_seq",
		"database_downloads_dl_id_seq",
		"database_licences_lic_id_seq",
		"database_uploads_up_id_seq",
		"db4s_connects_connect_id_seq",
		"discussion_comments_com_id_seq",
		"discussions_disc_id_seq",
		"email_queue_email_id_seq",
		"events_event_id_seq",
		"sql_terminal_history_history_id_seq",
		"sqlite_databases_db_id_seq",
		"users_user_id_seq",
		"vis_query_runs_query_run_id_seq",
	}

	// Begin a transaction
	tx, err := database.DB.Begin(context.Background())
	if err != nil {
		return err
	}
	// Set up an automatic transaction roll back if the function exits without committing
	defer tx.Rollback(context.Background())

	// Truncate the database tables
	for _, tbl := range tableNames {
		// Ugh, string smashing just feels so wrong when working with SQL
		dbQuery := fmt.Sprintf("TRUNCATE TABLE %s CASCADE", tbl)
		_, err := database.DB.Exec(context.Background(), dbQuery)
		if err != nil {
			log.Printf("Error truncating table while resetting database: %s", err)
			return err
		}
	}

	// Reset the sequences
	for _, seq := range sequenceNames {
		dbQuery := fmt.Sprintf("ALTER SEQUENCE %v RESTART", seq)
		_, err := database.DB.Exec(context.Background(), dbQuery)
		if err != nil {
			log.Printf("Error restarting sequence while resetting database: %v", err)
			return err
		}
	}

	// Add the default user to the system
	err = database.AddDefaultUser()
	if err != nil {
		log.Fatal(err)
	}

	// Add the default licences
	err = database.AddDefaultLicences()
	if err != nil {
		log.Fatal(err)
	}

	// Commit the transaction
	err = tx.Commit(context.Background())
	if err != nil {
		return err
	}

	// Log the database reset
	log.Println("Database reset")
	return nil
}

// SaveDBSettings saves updated database settings to PostgreSQL
func SaveDBSettings(userName, dbName, oneLineDesc, fullDesc, defaultTable string, public bool, sourceURL, defaultBranch string) error {
	// Check for values which should be NULL
	var nullable1LineDesc, nullableFullDesc, nullableSourceURL pgtype.Text
	if oneLineDesc == "" {
		nullable1LineDesc.Valid = false
	} else {
		nullable1LineDesc.String = oneLineDesc
		nullable1LineDesc.Valid = true
	}
	if fullDesc == "" {
		nullableFullDesc.Valid = false
	} else {
		nullableFullDesc.String = fullDesc
		nullableFullDesc.Valid = true
	}
	if sourceURL == "" {
		nullableSourceURL.Valid = false
	} else {
		nullableSourceURL.String = sourceURL
		nullableSourceURL.Valid = true
	}

	// Save the database settings
	SQLQuery := `
		UPDATE sqlite_databases
		SET one_line_description = $3, full_description = $4, default_table = $5, public = $6, source_url = $7,
			default_branch = $8
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
			)
			AND db_name = $2`
	commandTag, err := database.DB.Exec(context.Background(), SQLQuery, userName, dbName, nullable1LineDesc, nullableFullDesc, defaultTable,
		public, nullableSourceURL, defaultBranch)
	if err != nil {
		log.Printf("Updating description for database '%s/%s' failed: %v", SanitiseLogString(userName),
			SanitiseLogString(dbName), err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		errMsg := fmt.Sprintf("Wrong number of rows affected (%d) when updating description for '%s/%s'",
			numRows, userName, dbName)
		log.Printf(SanitiseLogString(errMsg))
		return errors.New(errMsg)
	}

	// Invalidate the old memcached entry for the database
	err = InvalidateCacheEntry(userName, userName, dbName, "") // Empty string indicates "for all versions"
	if err != nil {
		// Something went wrong when invalidating memcached entries for the database
		log.Printf("Error when invalidating memcache entries: %s", err.Error())
		return err
	}
	return nil
}

// SendEmails sends status update emails to people watching databases
func SendEmails() {
	// If the SMTP2Go API key hasn't been configured, there's no use in trying to send emails
	if config.Conf.Event.Smtp2GoKey == "" && os.Getenv("SMTP2GO_API_KEY") == "" {
		return
	}

	for {
		// Retrieve unsent emails from the email_queue
		type eml struct {
			Address string
			Body    string
			ID      int64
			Subject string
		}
		var emailList []eml
		dbQuery := `
				SELECT email_id, mail_to, subject, body
				FROM email_queue
				WHERE sent = false`
		rows, err := database.DB.Query(context.Background(), dbQuery)
		if err != nil {
			log.Printf("Database query failed: %v", err.Error())
			return // Abort, as we don't want to continuously resend the same emails
		}
		for rows.Next() {
			var oneRow eml
			err = rows.Scan(&oneRow.ID, &oneRow.Address, &oneRow.Subject, &oneRow.Body)
			if err != nil {
				log.Printf("Error retrieving queued emails: %v", err.Error())
				rows.Close()
				return // Abort, as we don't want to continuously resend the same emails
			}
			emailList = append(emailList, oneRow)
		}
		rows.Close()

		// Send emails
		for _, j := range emailList {
			e := smtp2go.Email{
				From:     "updates@dbhub.io",
				To:       []string{j.Address},
				Subject:  j.Subject,
				TextBody: j.Body,
				HtmlBody: j.Body,
			}
			_, err = smtp2go.Send(&e)
			if err != nil {
				log.Println(err)
			}

			log.Printf("Email with subject '%v' sent to '%v'",
				truncate.Truncate(j.Subject, 35, "...", truncate.PositionEnd), j.Address)

			// We only attempt delivery via smtp2go once (retries are handled on their end), so mark message as sent
			dbQuery := `
				UPDATE email_queue
				SET sent = true, sent_timestamp = now()
				WHERE email_id = $1`
			commandTag, err := database.DB.Exec(context.Background(), dbQuery, j.ID)
			if err != nil {
				log.Printf("Changing email status to sent failed for email '%v': '%v'", j.ID, err.Error())
				return // Abort, as we don't want to continuously resend the same emails
			}
			if numRows := commandTag.RowsAffected(); numRows != 1 {
				log.Printf("Wrong # of rows (%v) affected when changing email status to sent for email '%v'",
					numRows, j.ID)
			}
		}

		// Pause before running the loop again
		time.Sleep(config.Conf.Event.EmailQueueProcessingDelay * time.Second)
	}
}

// SocialStats returns the latest social stats for a given database
func SocialStats(dbOwner, dbName string) (wa, st, fo int, err error) {

	// TODO: Implement caching of these stats

	// Retrieve latest star, fork, and watcher count
	dbQuery := `
		SELECT stars, forks, watchers
		FROM sqlite_databases
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
			)
			AND db_name = $2`
	err = database.DB.QueryRow(context.Background(), dbQuery, dbOwner, dbName).Scan(&st, &fo, &wa)
	if err != nil {
		log.Printf("Error retrieving social stats count for '%s/%s': %v", SanitiseLogString(dbOwner),
			SanitiseLogString(dbName), err)
		return -1, -1, -1, err
	}
	return
}

// StatusUpdatesLoop periodically generates status updates (alert emails TBD) from the event queue
func StatusUpdatesLoop() {
	// Ensure a warning message is displayed on the console if the status update loop exits
	defer func() {
		log.Printf("%s: WARN: Status update loop exited", config.Conf.Live.Nodename)
	}()

	// Log the start of the loop
	log.Printf("%s: status update processing loop started.  %d second refresh.", config.Conf.Live.Nodename, config.Conf.Event.Delay)

	// Start the endless status update processing loop
	var err error
	type evEntry struct {
		dbID      int64
		details   database.EventDetails
		eType     database.EventType
		eventID   int64
		timeStamp time.Time
	}
	for {
		// Wait at the start of the loop (simpler code then adding a delay before each continue statement below)
		time.Sleep(config.Conf.Event.Delay * time.Second)

		// Begin a transaction
		var tx pgx.Tx
		tx, err = database.DB.Begin(context.Background())
		if err != nil {
			log.Printf("%s: couldn't begin database transaction for status update processing loop: %s",
				config.Conf.Live.Nodename, err.Error())
			continue
		}

		// Retrieve the list of outstanding events
		// NOTE - We gather the db_id here instead of dbOwner/dbName as it should be faster for PG to deal
		//        with when generating the watcher list
		dbQuery := `
			SELECT event_id, event_timestamp, db_id, event_type, event_data
			FROM events
			ORDER BY event_id ASC`
		rows, err := tx.Query(context.Background(), dbQuery)
		if err != nil {
			log.Printf("Generating status update event list failed: %v", err)
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) {
				log.Println(pgErr.Message)
				log.Println(pgErr.Code)
			}
			tx.Rollback(context.Background())
			continue
		}
		evList := make(map[int64]evEntry)
		for rows.Next() {
			var ev evEntry
			err = rows.Scan(&ev.eventID, &ev.timeStamp, &ev.dbID, &ev.eType, &ev.details)
			if err != nil {
				log.Printf("Error retrieving event list for status updates thread: %v", err)
				rows.Close()
				tx.Rollback(context.Background())
				continue
			}
			evList[ev.eventID] = ev
		}
		rows.Close()

		// For each event, add a status update to the status_updates list for each watcher it's for
		for id, ev := range evList {
			// Retrieve the list of watchers for the database the event occurred on
			dbQuery = `
				SELECT user_id
				FROM watchers
				WHERE db_id = $1`
			rows, err = tx.Query(context.Background(), dbQuery, ev.dbID)
			if err != nil {
				log.Printf("Error retrieving user list for status updates thread: %v", err)
				tx.Rollback(context.Background())
				continue
			}
			var users []int64
			for rows.Next() {
				var user int64
				err = rows.Scan(&user)
				if err != nil {
					log.Printf("Error retrieving user list for status updates thread: %v", err)
					rows.Close()
					tx.Rollback(context.Background())
					continue
				}
				users = append(users, user)
			}

			// For each watcher, add the new status update to their existing list
			// TODO: It might be better to store this list in Memcached instead of hitting the database like this
			for _, u := range users {
				// Retrieve the current status updates list for the user
				var eml pgtype.Text
				var userEvents map[string][]database.StatusUpdateEntry
				var userName string
				dbQuery = `
					SELECT user_name, email, status_updates
					FROM users
					WHERE user_id = $1`
				err = tx.QueryRow(context.Background(), dbQuery, u).Scan(&userName, &eml, &userEvents)
				if err != nil {
					if !errors.Is(err, pgx.ErrNoRows) {
						// A real error occurred
						log.Printf("Database query failed: %s", err)
						tx.Rollback(context.Background())
					}
					continue
				}
				if len(userEvents) == 0 {
					userEvents = make(map[string][]database.StatusUpdateEntry)
				}

				// If the user generated this event themselves, skip them
				if userName == ev.details.UserName {
					log.Printf("User '%v' generated this event (id: %v), so not adding it to their event list",
						userName, ev.eventID)
					continue
				}

				// * Add the new event to the users status updates list *

				// Group the status updates by database, and coalesce multiple updates for the same discussion or MR
				// into a single entry (keeping the most recent one of each)
				dbName := fmt.Sprintf("%s/%s", ev.details.Owner, ev.details.DBName)
				var a database.StatusUpdateEntry
				lst, ok := userEvents[dbName]
				if ev.details.Type == database.EVENT_NEW_DISCUSSION || ev.details.Type == database.EVENT_NEW_MERGE_REQUEST || ev.details.Type == database.EVENT_NEW_COMMENT {
					if ok {
						// Check if an entry already exists for the discussion/MR/comment
						for i, j := range lst {
							if j.DiscID == ev.details.DiscID {
								// Yes, there's already an existing entry for the discussion/MR/comment so delete the old entry
								lst = append(lst[:i], lst[i+1:]...) // Delete the old element
							}
						}
					}
				}

				// Add the new entry
				a.DiscID = ev.details.DiscID
				a.Title = ev.details.Title
				a.URL = ev.details.URL
				lst = append(lst, a)
				userEvents[dbName] = lst

				// Save the updated list for the user back to PG
				dbQuery = `
					UPDATE users
					SET status_updates = $2
					WHERE user_id = $1`
				commandTag, err := tx.Exec(context.Background(), dbQuery, u, userEvents)
				if err != nil {
					log.Printf("Adding status update for database ID '%d' to user id '%d' failed: %v", ev.dbID,
						u, err)
					tx.Rollback(context.Background())
					continue
				}
				if numRows := commandTag.RowsAffected(); numRows != 1 {
					log.Printf("Wrong number of rows affected (%d) when adding status update for database ID "+
						"'%d' to user id '%d'", numRows, ev.dbID, u)
					tx.Rollback(context.Background())
					continue
				}

				// Count the number of status updates for the user, to be displayed in the webUI header row
				var numUpdates int
				for _, i := range userEvents {
					numUpdates += len(i)
				}

				// Add an entry to memcached for the user, indicating they have outstanding status updates available
				err = SetUserStatusUpdates(userName, numUpdates)
				if err != nil {
					log.Printf("Error when updating user status updates # in memcached: %v", err)
					continue
				}

				// TODO: Add a email for the status notification to the outgoing email queue
				var msg, subj string
				switch ev.details.Type {
				case database.EVENT_NEW_DISCUSSION:
					msg = fmt.Sprintf("A new discussion has been created for %s/%s.\n\nVisit https://%s%s "+
						"for the details", ev.details.Owner, ev.details.DBName, config.Conf.Web.ServerName,
						ev.details.URL)
					subj = fmt.Sprintf("DBHub.io: New discussion created on %s/%s", ev.details.Owner,
						ev.details.DBName)
				case database.EVENT_NEW_MERGE_REQUEST:
					msg = fmt.Sprintf("A new merge request has been created for %s/%s.\n\nVisit https://%s%s "+
						"for the details", ev.details.Owner, ev.details.DBName, config.Conf.Web.ServerName,
						ev.details.URL)
					subj = fmt.Sprintf("DBHub.io: New merge request created on %s/%s", ev.details.Owner,
						ev.details.DBName)
				case database.EVENT_NEW_COMMENT:
					msg = fmt.Sprintf("A new comment has been created for %s/%s.\n\nVisit https://%s%s for "+
						"the details", ev.details.Owner, ev.details.DBName, config.Conf.Web.ServerName,
						ev.details.URL)
					subj = fmt.Sprintf("DBHub.io: New comment on %s/%s", ev.details.Owner,
						ev.details.DBName)
				default:
					log.Printf("Unknown message type when creating email message")
				}
				if eml.Valid {
					// If the email address is of the form username@this_server (which indicates a non-functional email address), then skip it
					serverName := strings.Split(config.Conf.Web.ServerName, ":")
					if strings.HasSuffix(eml.String, serverName[0]) {
						log.Printf("Skipping email '%v' to destination '%v', as it ends in '%v'",
							truncate.Truncate(subj, 35, "...", truncate.PositionEnd), eml.String, serverName[0])
						continue
					}

					// Add the email to the queue
					dbQuery = `
						INSERT INTO email_queue (mail_to, subject, body)
						VALUES ($1, $2, $3)`
					commandTag, err = tx.Exec(context.Background(), dbQuery, eml.String, subj, msg)
					if err != nil {
						log.Printf("Adding status update to email queue for user '%v' failed: %v", u, err)
						tx.Rollback(context.Background())
						continue
					}
					if numRows := commandTag.RowsAffected(); numRows != 1 {
						log.Printf("Wrong number of rows affected (%d) when adding status update to email"+
							"queue for user '%v'", numRows, u)
						tx.Rollback(context.Background())
						continue
					}
				}
			}

			// Remove the processed event from PG
			dbQuery = `
				DELETE FROM events
				WHERE event_id = $1`
			commandTag, err := tx.Exec(context.Background(), dbQuery, id)
			if err != nil {
				log.Printf("Removing event ID '%d' failed: %v", id, err)
				continue
			}
			if numRows := commandTag.RowsAffected(); numRows != 1 {
				log.Printf("Wrong number of rows affected (%d) when removing event ID '%d'", numRows, id)
				continue
			}
		}

		// Commit the transaction
		err = tx.Commit(context.Background())
		if err != nil {
			log.Printf("Could not commit transaction when processing status updates: %v", err.Error())
			continue
		}
	}
	return
}

// StoreBranches updates the branches list for a database
func StoreBranches(dbOwner, dbName string, branches map[string]BranchEntry) error {
	dbQuery := `
		UPDATE sqlite_databases
		SET branch_heads = $3, branches = $4
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
				)
			AND db_name = $2`
	commandTag, err := database.DB.Exec(context.Background(), dbQuery, dbOwner, dbName, branches, len(branches))
	if err != nil {
		log.Printf("Updating branch heads for database '%s/%s' to '%v' failed: %v",
			SanitiseLogString(dbOwner), SanitiseLogString(dbName), branches, err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf(
			"Wrong number of rows (%d) affected when updating branch heads for database '%s/%s' to '%v'",
			numRows, SanitiseLogString(dbOwner), SanitiseLogString(dbName), branches)
	}
	return nil
}

// StoreCommits updates the commit list for a database
func StoreCommits(dbOwner, dbName string, commitList map[string]database.CommitEntry) error {
	dbQuery := `
		UPDATE sqlite_databases
		SET commit_list = $3, last_modified = now()
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
				)
			AND db_name = $2`
	commandTag, err := database.DB.Exec(context.Background(), dbQuery, dbOwner, dbName, commitList)
	if err != nil {
		log.Printf("Updating commit list for database '%s/%s' failed: %v", SanitiseLogString(dbOwner),
			SanitiseLogString(dbName), err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("Wrong number of rows (%d) affected when updating commit list for database '%s/%s'", numRows,
			SanitiseLogString(dbOwner), SanitiseLogString(dbName))
	}
	return nil
}

// StoreDatabase stores database details in PostgreSQL, and the database data itself in Minio
func StoreDatabase(dbOwner, dbName string, branches map[string]BranchEntry, c database.CommitEntry, pub bool,
	buf *os.File, sha string, dbSize int64, oneLineDesc, fullDesc string, createDefBranch bool, branchName,
	sourceURL string) error {
	// Store the database file
	err := StoreDatabaseFile(buf, sha, dbSize)
	if err != nil {
		return err
	}

	// Check for values which should be NULL
	var nullable1LineDesc, nullableFullDesc pgtype.Text
	if oneLineDesc == "" {
		nullable1LineDesc.Valid = false
	} else {
		nullable1LineDesc.String = oneLineDesc
		nullable1LineDesc.Valid = true
	}
	if fullDesc == "" {
		nullableFullDesc.Valid = false
	} else {
		nullableFullDesc.String = fullDesc
		nullableFullDesc.Valid = true
	}

	// Store the database metadata
	cMap := map[string]database.CommitEntry{c.ID: c}
	var commandTag pgconn.CommandTag
	dbQuery := `
		WITH root AS (
			SELECT nextval('sqlite_databases_db_id_seq') AS val
		)
		INSERT INTO sqlite_databases (user_id, db_id, db_name, public, one_line_description, full_description,
			branch_heads, root_database, commit_list`
	if sourceURL != "" {
		dbQuery += `, source_url`
	}
	dbQuery +=
		`)
		SELECT (
			SELECT user_id
			FROM users
			WHERE lower(user_name) = lower($1)), (SELECT val FROM root), $2, $3, $4, $5, $7, (SELECT val FROM root), $6`
	if sourceURL != "" {
		dbQuery += `, $8`
	}
	dbQuery += `
		ON CONFLICT (user_id, db_name)
			DO UPDATE
			SET commit_list = sqlite_databases.commit_list || $6,
				branch_heads = sqlite_databases.branch_heads || $7,
				last_modified = now()`
	if sourceURL != "" {
		dbQuery += `,
			source_url = $8`
		commandTag, err = database.DB.Exec(context.Background(), dbQuery, dbOwner, dbName, pub, nullable1LineDesc, nullableFullDesc,
			cMap, branches, sourceURL)
	} else {
		commandTag, err = database.DB.Exec(context.Background(), dbQuery, dbOwner, dbName, pub, nullable1LineDesc, nullableFullDesc,
			cMap, branches)
	}
	if err != nil {
		log.Printf("Storing database '%s/%s' failed: %v", SanitiseLogString(dbOwner),
			SanitiseLogString(dbName), err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("Wrong number of rows (%d) affected while storing database '%s/%s'", numRows, SanitiseLogString(dbOwner),
			SanitiseLogString(dbName))
	}

	if createDefBranch {
		err = StoreDefaultBranchName(dbOwner, dbName, branchName)
		if err != nil {
			log.Printf("Storing default branch '%s' name for '%s/%s' failed: %v", SanitiseLogString(branchName),
				SanitiseLogString(dbOwner), SanitiseLogString(dbName), err)
			return err
		}
	}
	return nil
}

// StoreDefaultBranchName stores the default branch name for a database
func StoreDefaultBranchName(dbOwner, dbName, branchName string) error {
	dbQuery := `
		UPDATE sqlite_databases
		SET default_branch = $3
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
				)
			AND db_name = $2`
	commandTag, err := database.DB.Exec(context.Background(), dbQuery, dbOwner, dbName, branchName)
	if err != nil {
		log.Printf("Changing default branch for database '%v' to '%v' failed: %v", SanitiseLogString(dbName),
			SanitiseLogString(branchName), err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("Wrong number of rows (%d) affected during update: database: %v, new branch name: '%v'",
			numRows, SanitiseLogString(dbName), SanitiseLogString(branchName))
	}
	return nil
}

// StoreDefaultTableName stores the default table name for a database
func StoreDefaultTableName(dbOwner, dbName, tableName string) error {
	var t pgtype.Text
	if tableName != "" {
		t.String = tableName
		t.Valid = true
	}
	dbQuery := `
		UPDATE sqlite_databases
		SET default_table = $3
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
				)
			AND db_name = $2`
	commandTag, err := database.DB.Exec(context.Background(), dbQuery, dbOwner, dbName, t)
	if err != nil {
		log.Printf("Changing default table for database '%v' to '%v' failed: %v", SanitiseLogString(dbName),
			tableName, err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("Wrong number of rows (%d) affected during update: database: %v, new table name: '%v'",
			numRows, SanitiseLogString(dbName), tableName)
	}
	return nil
}

// StoreReleases stores the releases for a database
func StoreReleases(dbOwner, dbName string, releases map[string]ReleaseEntry) error {
	dbQuery := `
		UPDATE sqlite_databases
		SET release_list = $3, release_count = $4
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
			)
			AND db_name = $2`
	commandTag, err := database.DB.Exec(context.Background(), dbQuery, dbOwner, dbName, releases, len(releases))
	if err != nil {
		log.Printf("Storing releases for database '%s/%s' failed: %v", SanitiseLogString(dbOwner),
			SanitiseLogString(dbName), err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("Wrong number of rows (%d) affected when storing releases for database: '%s/%s'", numRows,
			SanitiseLogString(dbOwner), SanitiseLogString(dbName))
	}
	return nil
}

// StoreTags stores the tags for a database
func StoreTags(dbOwner, dbName string, tags map[string]TagEntry) error {
	dbQuery := `
		UPDATE sqlite_databases
		SET tag_list = $3, tags = $4
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
			)
			AND db_name = $2`
	commandTag, err := database.DB.Exec(context.Background(), dbQuery, dbOwner, dbName, tags, len(tags))
	if err != nil {
		log.Printf("Storing tags for database '%s/%s' failed: %v", SanitiseLogString(dbOwner),
			SanitiseLogString(dbName), err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("Wrong number of rows (%d) affected when storing tags for database: '%s/%s'", numRows,
			SanitiseLogString(dbOwner), SanitiseLogString(dbName))
	}
	return nil
}

// UpdateContributorsCount updates the contributors count for a database
func UpdateContributorsCount(dbOwner, dbName string) error {
	// Get the commit list for the database
	commitList, err := GetCommitList(dbOwner, dbName)
	if err != nil {
		return err
	}

	// Work out the new contributor count
	d := map[string]struct{}{}
	for _, k := range commitList {
		d[k.AuthorEmail] = struct{}{}
	}
	n := len(d)

	// Store the new contributor count in the database
	dbQuery := `
		UPDATE sqlite_databases
		SET contributors = $3
			WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
			)
				AND db_name = $2`
	commandTag, err := database.DB.Exec(context.Background(), dbQuery, dbOwner, dbName, n)
	if err != nil {
		log.Printf("Updating contributor count in database '%s/%s' failed: %v", SanitiseLogString(dbOwner),
			SanitiseLogString(dbName), err)
		return err
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("Wrong # of rows affected (%v) when updating contributor count for database '%s/%s'",
			numRows, SanitiseLogString(dbOwner), SanitiseLogString(dbName))
	}
	return nil
}

// UpdateModified is a simple function to change the 'last modified' timestamp for a database to now()
func UpdateModified(dbOwner, dbName string) (err error) {
	dbQuery := `
		UPDATE sqlite_databases AS db
		SET last_modified = now()
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
			)
			AND db_name = $2`
	commandTag, err := database.DB.Exec(context.Background(), dbQuery, dbOwner, dbName)
	if err != nil {
		log.Printf("%s: updating last_modified for database '%s/%s' failed: %v", config.Conf.Live.Nodename, dbOwner,
			dbName, err)
		return
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("%s: wrong number of rows (%d) affected when updating last_modified for database '%s/%s'",
			config.Conf.Live.Nodename, numRows, dbOwner, dbName)
	}
	return
}

// UserDBs returns the list of databases for a user
func UserDBs(userName string, public AccessType) (list []DBInfo, err error) {
	// Construct SQL query for retrieving the requested database list
	dbQuery := `
		WITH u AS (
			SELECT user_id
			FROM users
			WHERE lower(user_name) = lower($1)
		), default_commits AS (
			SELECT DISTINCT ON (db.db_name) db_name, db.db_id, db.branch_heads->db.default_branch->>'commit' AS id
			FROM sqlite_databases AS db, u
			WHERE db.user_id = u.user_id
		), dbs AS (
			SELECT DISTINCT ON (db.db_name) db.db_name, db.date_created, db.last_modified, db.public,
				db.watchers, db.stars, db.discussions, db.merge_requests, db.branches, db.release_count, db.tags,
				db.contributors, db.one_line_description, default_commits.id,
				db.commit_list->default_commits.id->'tree'->'entries'->0, db.source_url, db.default_branch,
				db.download_count, db.page_views
			FROM sqlite_databases AS db, default_commits
			WHERE db.db_id = default_commits.db_id
				AND db.is_deleted = false
				AND db.live_db = false`
	switch public {
	case DB_PUBLIC:
		// Only public databases
		dbQuery += ` AND db.public = true`
	case DB_PRIVATE:
		// Only private databases
		dbQuery += ` AND db.public = false`
	case DB_BOTH:
		// Both public and private, so no need to add a query clause
	default:
		// This clause shouldn't ever be reached
		return nil, fmt.Errorf("Incorrect 'public' value '%v' passed to UserDBs() function.", public)
	}
	dbQuery += `
		)
		SELECT *
		FROM dbs
		ORDER BY last_modified DESC`
	rows, err := database.DB.Query(context.Background(), dbQuery, userName)
	if err != nil {
		log.Printf("Getting list of databases for user failed: %s", err)
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var defBranch, desc, source pgtype.Text
		var oneRow DBInfo
		err = rows.Scan(&oneRow.Database, &oneRow.DateCreated, &oneRow.RepoModified, &oneRow.Public,
			&oneRow.Watchers, &oneRow.Stars, &oneRow.Discussions, &oneRow.MRs, &oneRow.Branches,
			&oneRow.Releases, &oneRow.Tags, &oneRow.Contributors, &desc, &oneRow.CommitID, &oneRow.DBEntry, &source,
			&defBranch, &oneRow.Downloads, &oneRow.Views)
		if err != nil {
			log.Printf("Error retrieving database list for user: %v", err)
			return nil, err
		}
		if defBranch.Valid {
			oneRow.DefaultBranch = defBranch.String
		}
		if desc.Valid {
			oneRow.OneLineDesc = desc.String
		}
		if source.Valid {
			oneRow.SourceURL = source.String
		}
		oneRow.LastModified = oneRow.DBEntry.LastModified
		oneRow.Size = oneRow.DBEntry.Size
		oneRow.SHA256 = oneRow.DBEntry.Sha256

		// Work out the licence name and url for the database entry
		licSHA := oneRow.DBEntry.LicenceSHA
		if licSHA != "" {
			oneRow.Licence, oneRow.LicenceURL, err = database.GetLicenceInfoFromSha256(userName, licSHA)
			if err != nil {
				return nil, err
			}
		} else {
			oneRow.Licence = "Not specified"
		}
		list = append(list, oneRow)
	}

	// Get fork count for each of the databases
	for i, j := range list {
		// Retrieve the latest fork count
		dbQuery = `
			WITH u AS (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
			)
			SELECT forks
			FROM sqlite_databases, u
			WHERE db_id = (
				SELECT root_database
				FROM sqlite_databases
				WHERE user_id = u.user_id
					AND db_name = $2)`
		err = database.DB.QueryRow(context.Background(), dbQuery, userName, j.Database).Scan(&list[i].Forks)
		if err != nil {
			log.Printf("Error retrieving fork count for '%s/%s': %v", SanitiseLogString(userName),
				j.Database, err)
			return nil, err
		}
	}
	return list, nil
}

// UserStarredDBs returns the list of databases starred by a user
func UserStarredDBs(userName string) (list []database.DBEntry, err error) {
	dbQuery := `
		WITH u AS (
			SELECT user_id
			FROM users
			WHERE lower(user_name) = lower($1)
		),
		stars AS (
			SELECT st.db_id, st.date_starred
			FROM database_stars AS st, u
			WHERE st.user_id = u.user_id
		),
		db_users AS (
			SELECT db.user_id, db.db_name, stars.date_starred
			FROM sqlite_databases AS db, stars
			WHERE db.db_id = stars.db_id
			AND db.is_deleted = false
		)
		SELECT users.user_name, db_users.db_name, db_users.date_starred
		FROM users, db_users
		WHERE users.user_id = db_users.user_id
		ORDER BY date_starred DESC`
	rows, err := database.DB.Query(context.Background(), dbQuery, userName)
	if err != nil {
		log.Printf("Database query failed: %v", err)
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var oneRow database.DBEntry
		err = rows.Scan(&oneRow.Owner, &oneRow.DBName, &oneRow.DateEntry)
		if err != nil {
			log.Printf("Error retrieving stars list for user: %v", err)
			return nil, err
		}
		list = append(list, oneRow)
	}

	return list, nil
}

// UserWatchingDBs returns the list of databases watched by a user
func UserWatchingDBs(userName string) (list []database.DBEntry, err error) {
	dbQuery := `
		WITH u AS (
			SELECT user_id
			FROM users
			WHERE lower(user_name) = lower($1)
		),
		watching AS (
			SELECT w.db_id, w.date_watched
			FROM watchers AS w, u
			WHERE w.user_id = u.user_id
		),
		db_users AS (
			SELECT db.user_id, db.db_name, watching.date_watched
			FROM sqlite_databases AS db, watching
			WHERE db.db_id = watching.db_id
			AND db.is_deleted = false
		)
		SELECT users.user_name, db_users.db_name, db_users.date_watched
		FROM users, db_users
		WHERE users.user_id = db_users.user_id
		ORDER BY date_watched DESC`
	rows, err := database.DB.Query(context.Background(), dbQuery, userName)
	if err != nil {
		log.Printf("Database query failed: %v", err)
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var oneRow database.DBEntry
		err = rows.Scan(&oneRow.Owner, &oneRow.DBName, &oneRow.DateEntry)
		if err != nil {
			log.Printf("Error retrieving database watch list for user: %v", err)
			return nil, err
		}
		list = append(list, oneRow)
	}

	return list, nil
}

// ViewCount returns the view counter for a specific database
func ViewCount(dbOwner, dbName string) (viewCount int, err error) {
	dbQuery := `
		SELECT page_views
		FROM sqlite_databases
		WHERE user_id = (
				SELECT user_id
				FROM users
				WHERE lower(user_name) = lower($1)
			)
			AND db_name = $2`
	err = database.DB.QueryRow(context.Background(), dbQuery, dbOwner, dbName).Scan(&viewCount)
	if err != nil {
		log.Printf("Retrieving view count for '%s/%s' failed: %v", SanitiseLogString(dbOwner), SanitiseLogString(dbName), err)
		return 0, err
	}
	return
}
