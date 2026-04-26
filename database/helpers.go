package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func normalizeDriver(driver string) string {
	driver = strings.TrimSpace(strings.ToLower(driver))
	if driver == "" {
		return "postgres"
	}
	return driver
}

func parseDBTimeValue(value interface{}) (time.Time, error) {
	switch v := value.(type) {
	case nil:
		return time.Time{}, nil
	case time.Time:
		return v, nil
	case string:
		return parseDBTimeString(v)
	case []byte:
		return parseDBTimeString(string(v))
	default:
		return time.Time{}, fmt.Errorf("不支持的时间类型: %T", value)
	}
}

func parseDBNullTimeValue(value interface{}) (sql.NullTime, error) {
	if value == nil {
		return sql.NullTime{}, nil
	}
	t, err := parseDBTimeValue(value)
	if err != nil {
		return sql.NullTime{}, err
	}
	if t.IsZero() {
		return sql.NullTime{}, nil
	}
	return sql.NullTime{Time: t, Valid: true}, nil
}

func parseDBTimeString(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, value); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("无法解析时间值: %q", value)
}

func sqliteTimeParam(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format("2006-01-02 15:04:05")
}

func (db *DB) timeRangeArgs(start, end time.Time) (interface{}, interface{}) {
	if db != nil && db.isSQLite() {
		return sqliteTimeParam(start), sqliteTimeParam(end)
	}
	return start, end
}

func (db *DB) timeArg(value time.Time) interface{} {
	if db != nil && db.isSQLite() {
		return sqliteTimeParam(value)
	}
	return value
}

func decodeCredentials(raw interface{}) map[string]interface{} {
	data := bytesFromDBValue(raw)
	if len(data) == 0 {
		return map[string]interface{}{}
	}

	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		return map[string]interface{}{}
	}
	if out == nil {
		return map[string]interface{}{}
	}
	return out
}

func bytesFromDBValue(raw interface{}) []byte {
	switch v := raw.(type) {
	case nil:
		return nil
	case []byte:
		return append([]byte(nil), v...)
	case string:
		return []byte(v)
	default:
		return []byte(fmt.Sprint(v))
	}
}

func mergeCredentialMaps(base map[string]interface{}, updates map[string]interface{}) map[string]interface{} {
	if base == nil {
		base = map[string]interface{}{}
	}
	for key, value := range updates {
		base[key] = value
	}
	return base
}

func credentialString(raw interface{}, key string) string {
	credentials := decodeCredentials(raw)
	if credentials == nil {
		return ""
	}
	value, ok := credentials[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return fmt.Sprintf("%v", typed)
	default:
		return fmt.Sprintf("%v", typed)
	}
}

func accountEmailFromRawCredentials(raw interface{}) string {
	return credentialString(raw, "email")
}

func (db *DB) isSQLite() bool {
	return db != nil && db.driver == "sqlite"
}

func (db *DB) Driver() string {
	if db == nil {
		return "postgres"
	}
	return db.driver
}

func (db *DB) Label() string {
	if db.isSQLite() {
		return "SQLite"
	}
	return "PostgreSQL"
}

func (db *DB) SetMaxOpenConns(n int) {
	if db == nil || db.conn == nil {
		return
	}
	if db.isSQLite() {
		// SQLite 单文件模式下保持单连接，避免写锁竞争。
		db.conn.SetMaxOpenConns(1)
		db.conn.SetMaxIdleConns(1)
		return
	}
	db.conn.SetMaxOpenConns(n)
	db.conn.SetMaxIdleConns(n / 2)
}

func (db *DB) insertRowID(ctx context.Context, postgresQuery string, sqliteQuery string, args ...interface{}) (int64, error) {
	if db.isSQLite() {
		res, err := db.conn.ExecContext(ctx, sqliteQuery, args...)
		if err != nil {
			return 0, err
		}
		affected, err := res.RowsAffected()
		if err == nil && affected == 0 {
			return 0, sql.ErrNoRows
		}
		return res.LastInsertId()
	}

	var id int64
	err := db.conn.QueryRowContext(ctx, postgresQuery, args...).Scan(&id)
	return id, err
}
