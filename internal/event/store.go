package event

import (
	"database/sql"
	"encoding/json"
)

const eventColumns = `id, action, trace_id, cortex_id, origin, timestamp, data, vclock`

// Append inserts an event into the event log within an existing transaction.
func Append(tx *sql.Tx, e *Event) error {
	vclock, err := json.Marshal(e.VClock)
	if err != nil {
		return err
	}
	data := e.Data
	if data == nil {
		data = json.RawMessage("{}")
	}
	_, err = tx.Exec(
		`INSERT INTO events (`+eventColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, string(e.Action), e.TraceID, e.CortexID, e.Origin, e.Timestamp, string(data), string(vclock),
	)
	return err
}

// Since returns events with ID > afterID, ordered chronologically, up to limit.
// If afterID is "", returns from the beginning.
func Since(db *sql.DB, afterID string, limit int) ([]Event, error) {
	var q string
	var args []any
	if afterID == "" {
		q = `SELECT ` + eventColumns + ` FROM events ORDER BY id LIMIT ?`
		args = []any{limit}
	} else {
		q = `SELECT ` + eventColumns + ` FROM events WHERE id > ? ORDER BY id LIMIT ?`
		args = []any{afterID, limit}
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

// ForTrace returns all events for a given trace ID, ordered chronologically.
func ForTrace(db *sql.DB, traceID string) ([]Event, error) {
	rows, err := db.Query(
		`SELECT `+eventColumns+` FROM events WHERE trace_id = ? ORDER BY id`,
		traceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

func scanEvents(rows *sql.Rows) ([]Event, error) {
	var events []Event
	for rows.Next() {
		var e Event
		var data, vclock string
		if err := rows.Scan(&e.ID, &e.Action, &e.TraceID, &e.CortexID, &e.Origin, &e.Timestamp, &data, &vclock); err != nil {
			return nil, err
		}
		e.Data = json.RawMessage(data)
		if err := json.Unmarshal([]byte(vclock), &e.VClock); err != nil {
			e.VClock = nil
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
