package fedrooms

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// inTx runs fn in a transaction and commits it.
func inTx(t *testing.T, db *sql.DB, fn func(tx *sql.Tx) error) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// join adds address's stand-in to room with rights and logs it.
func join(t *testing.T, db *sql.DB, room, address, rights, domain string) int64 {
	t.Helper()
	var id int64
	inTx(t, db, func(tx *sql.Tx) error {
		var err error
		if id, err = StandIn(context.Background(), tx, address); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO room_members(room_id, key_id, role, joined_at, rights) VALUES (?, ?, 'reader', '2026-01-01T00:00:00Z', ?)`,
			room, id, rights); err != nil {
			return err
		}
		return LogJoin(context.Background(), tx, room, id, domain)
	})
	return id
}

func events(t *testing.T, db *sql.DB, room string) []Event {
	t.Helper()
	rows, err := db.Query(`SELECT seq, type, payload FROM fed_events WHERE room_id = ? ORDER BY seq`, room)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var p string
		if err := rows.Scan(&e.Seq, &e.Type, &p); err != nil {
			t.Fatal(err)
		}
		e.Payload = json.RawMessage(p)
		out = append(out, e)
	}
	for i, e := range out {
		if e.Seq != int64(i+1) {
			t.Fatalf("room %s: seq %d at position %d, want no gaps", room, e.Seq, i)
		}
	}
	return out
}

// hostRoom is a room on a.example with an admin owner that is the
// operator key, a bot, a key without a handle and n messages.
type hostRoom struct {
	db                 *sql.DB
	owner, bot, plain  int64
	firstMsg, operator int64
}

func newHostRoom(t *testing.T, room string, n int) hostRoom {
	t.Helper()
	db := testDB(t)
	h := hostRoom{db: db}
	h.owner = addKey(t, db, `INSERT INTO api_keys(label, hash, prefix, created_at, is_admin) VALUES ('op', 'h', 'p0', '2026-01-01T00:00:00Z', 1)`)
	if _, err := db.Exec(`INSERT INTO meta(key, value) VALUES (?, ?)`, OperatorMetaKey, strconv.FormatInt(h.owner, 10)); err != nil {
		t.Fatal(err)
	}
	h.bot = addKey(t, db, `INSERT INTO api_keys(label, hash, prefix, created_at, handle) VALUES ('bot', 'h', 'p1', '2026-01-01T00:00:00Z', 'b1')`)
	h.plain = addKey(t, db, `INSERT INTO api_keys(label, hash, prefix, created_at) VALUES ('plain', 'h', 'p2', '2026-01-01T00:00:00Z')`)
	for _, q := range []string{
		fmt.Sprintf(`INSERT INTO rooms(id, name, owner_key_id, created_at) VALUES ('%s', 'Room %s', %d, '2026-01-01T00:00:00Z')`, room, room, h.owner),
		fmt.Sprintf(`INSERT INTO room_members(room_id, key_id, role, joined_at, rights) VALUES ('%s', %d, 'owner', '2026-01-01T00:00:00Z', 'read,claim,system')`, room, h.owner),
		fmt.Sprintf(`INSERT INTO room_members(room_id, key_id, role, joined_at, rights) VALUES ('%s', %d, 'reader', '2026-01-01T00:00:00Z', 'read,answer')`, room, h.bot),
		fmt.Sprintf(`INSERT INTO room_members(room_id, key_id, role, joined_at, rights) VALUES ('%s', %d, 'reader', '2026-01-01T00:00:00Z', '')`, room, h.plain),
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= n; i++ {
		res, err := db.Exec(`INSERT INTO messages(room_id, author_key_id, ts, content, content_type, message_type, trust_origin)
			VALUES (?, ?, ?, ?, 'text/plain', 'answer', 'internal')`, room, h.bot, fmt.Sprintf("2026-01-01T00:00:00.%09dZ", i), "m"+strconv.Itoa(i))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		if i == 1 {
			h.firstMsg = id
		}
	}
	// The newest but one is deleted; the newest is the operator's, a reply.
	if _, err := db.Exec(`UPDATE messages SET deleted_at = '2026-01-02T00:00:00Z' WHERE room_id = ? AND content = ?`, room, "m"+strconv.Itoa(n)); err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`INSERT INTO messages(room_id, author_key_id, ts, content, content_type, message_type, metadata, trust_origin, operator)
		VALUES (?, ?, '2026-01-03T00:00:00.000000000Z', 'stop', 'text/markdown', 'decision', '{"z":1,"reply_to":5}', 'internal', 1)`, room, h.owner)
	if err != nil {
		t.Fatal(err)
	}
	h.operator, _ = res.LastInsertId()
	return h
}

func TestLogJoin_Snapshot(t *testing.T) {
	h := newHostRoom(t, "R", 202)
	if got := events(t, h.db, "R"); len(got) != 0 {
		t.Fatalf("events before a remote member: %v", got)
	}
	join(t, h.db, "R", "c1@c.example", "read,question", "a.example")
	got := events(t, h.db, "R")
	// room, three members (the admin key left out), 200 messages.
	if len(got) != 1+3+SnapshotMessages {
		t.Fatalf("snapshot has %d events, want %d", len(got), 1+3+SnapshotMessages)
	}
	want := []string{
		`{"name":"Room R","archived":false}`,
		`{"address":"b1@a.example","rights":["read","answer"]}`,
		`{"address":"key-` + strconv.FormatInt(h.plain, 10) + `@a.example","rights":[]}`,
		`{"address":"c1@c.example","rights":["read","question"]}`,
	}
	for i, w := range want {
		typ := EventMember
		if i == 0 {
			typ = EventRoom
		}
		if got[i].Type != typ || string(got[i].Payload) != w {
			t.Errorf("event %d: %s %s, want %s %s", i+1, got[i].Type, got[i].Payload, typ, w)
		}
	}
	msgs := got[4:]
	var ids []int64
	for _, e := range msgs {
		var p MessagePayload
		if e.Type != EventMessage || json.Unmarshal(e.Payload, &p) != nil {
			t.Fatalf("event %d: %s %s", e.Seq, e.Type, e.Payload)
		}
		if strings.Contains(string(e.Payload), "operator\"") || strings.Contains(string(e.Payload), `"trust_origin"`) {
			t.Errorf("event %d carries a local mark: %s", e.Seq, e.Payload)
		}
		ids = append(ids, p.ID)
	}
	// The latest 200 that are not deleted, oldest first: m3..m201, then
	// the operator's message.
	if ids[0] != h.firstMsg+2 || ids[198] != h.firstMsg+200 || ids[199] != h.operator {
		t.Errorf("snapshot messages: first %d, 199th %d, last %d", ids[0], ids[198], ids[199])
	}
	first := `{"id":` + strconv.FormatInt(h.firstMsg+2, 10) + `,"author":"b1@a.example","ts":"2026-01-01T00:00:00.000000003Z",` +
		`"content_type":"text/plain","message_type":"answer","content":"m3"}`
	if string(msgs[0].Payload) != first {
		t.Errorf("first message:\n got %s\nwant %s", msgs[0].Payload, first)
	}
	// The operator's message: an ordinary message of operator@host,
	// metadata without reply_to, which travels in its own field.
	last := `{"id":` + strconv.FormatInt(h.operator, 10) + `,"author":"operator@a.example","ts":"2026-01-03T00:00:00.000000000Z",` +
		`"content_type":"text/markdown","message_type":"decision","content":"stop","metadata":{"z":1},"reply_to":5}`
	if string(msgs[199].Payload) != last {
		t.Errorf("operator message:\n got %s\nwant %s", msgs[199].Payload, last)
	}
	var acked int
	if err := h.db.QueryRow(`SELECT acked_seq FROM fed_delivery WHERE room_id = 'R' AND peer = 'c.example'`).Scan(&acked); err != nil || acked != 0 {
		t.Errorf("delivery row for c.example: acked %d, err %v", acked, err)
	}
}

func TestLog_OnlyRoomsWithStandIns(t *testing.T) {
	h := newHostRoom(t, "R", 2)
	ctx := context.Background()
	logAll := func(domain string) {
		inTx(t, h.db, func(tx *sql.Tx) error {
			for _, err := range []error{
				LogMember(ctx, tx, "R", h.bot, domain),
				LogMessage(ctx, tx, "R", h.firstMsg, domain, 256<<10),
				LogRoom(ctx, tx, "R", domain),
				LogRemove(ctx, tx, "R", h.plain, domain),
				LogJoin(ctx, tx, "R", h.plain, domain),
			} {
				if err != nil {
					return err
				}
			}
			return nil
		})
	}
	logAll("a.example")
	if got := events(t, h.db, "R"); len(got) != 0 {
		t.Fatalf("room without a stand-in: %d events", len(got))
	}
	join(t, h.db, "R", "c1@c.example", "read", "a.example")
	n := len(events(t, h.db, "R"))
	// Federation off: nothing is logged, not even in a room with a stand-in.
	logAll("")
	if got := len(events(t, h.db, "R")); got != n {
		t.Fatalf("domain off: %d events, want %d", got, n)
	}
	logAll("a.example")
	got := events(t, h.db, "R")[n:]
	var types []string
	for _, e := range got {
		types = append(types, e.Type)
	}
	if strings.Join(types, " ") != "member message room member.remove member" {
		t.Errorf("logged: %v", types)
	}
	if string(got[3].Payload) != `{"address":"key-`+strconv.FormatInt(h.plain, 10)+`@a.example"}` {
		t.Errorf("member.remove payload: %s", got[3].Payload)
	}
	// The admin owner's changes are not logged.
	inTx(t, h.db, func(tx *sql.Tx) error { return LogMember(ctx, tx, "R", h.owner, "a.example") })
	if got := len(events(t, h.db, "R")); got != n+5 {
		t.Errorf("admin key logged: %d events, want %d", got, n+5)
	}
}

func TestLog_SecondStandInAndLastLeaving(t *testing.T) {
	h := newHostRoom(t, "R", 1)
	ctx := context.Background()
	c1 := join(t, h.db, "R", "c1@c.example", "read", "a.example")
	n := len(events(t, h.db, "R"))
	// A second remote member gets its member event, no new snapshot.
	d1 := join(t, h.db, "R", "d1@d.example", "read", "a.example")
	got := events(t, h.db, "R")
	if len(got) != n+1 || got[n].Type != EventMember || string(got[n].Payload) != `{"address":"d1@d.example","rights":["read"]}` {
		t.Fatalf("second stand-in: %v", got[n:])
	}
	// The last stand-in leaving is logged, so its server learns it.
	for _, id := range []int64{c1, d1} {
		inTx(t, h.db, func(tx *sql.Tx) error {
			if _, err := tx.Exec(`DELETE FROM room_members WHERE room_id = 'R' AND key_id = ?`, id); err != nil {
				return err
			}
			return LogRemove(ctx, tx, "R", id, "a.example")
		})
	}
	got = events(t, h.db, "R")
	if len(got) != n+3 || got[n+2].Type != EventMemberRemove || string(got[n+2].Payload) != `{"address":"d1@d.example"}` {
		t.Fatalf("removals: %v", got[n:])
	}
	// Then the room is local again.
	inTx(t, h.db, func(tx *sql.Tx) error { return LogRoom(ctx, tx, "R", "a.example") })
	if len(events(t, h.db, "R")) != n+3 {
		t.Error("room without stand-ins still logged")
	}
}

func TestLog_SeqPerRoom(t *testing.T) {
	h := newHostRoom(t, "R", 1)
	if _, err := h.db.Exec(`INSERT INTO rooms(id, name, owner_key_id, created_at) VALUES ('S', 'S', ?, '2026-01-01T00:00:00Z')`, h.owner); err != nil {
		t.Fatal(err)
	}
	join(t, h.db, "R", "c1@c.example", "read", "a.example")
	join(t, h.db, "S", "c1@c.example", "read", "a.example")
	if got := events(t, h.db, "S"); len(got) != 2 || got[0].Seq != 1 {
		t.Errorf("room S: %d events", len(got)) // room, member c1, no messages
	}
}

func TestSplitMetadata(t *testing.T) {
	five := int64(5)
	for _, c := range []struct {
		in, wire string
		reply    *int64
		bad      bool
	}{
		{"", "", nil, false},
		{"null", "", nil, false},
		{"{}", "", nil, false},
		{` { } `, "", nil, false},
		{`{"reply_to":5}`, "", &five, false},
		{`{"b":1,"a":{"y":2, "x":[1, 2]}}`, `{"a":{"y":2,"x":[1,2]},"b":1}`, nil, false},
		{`{"reply_to":5,"b":"<&>","a":1}`, `{"a":1,"b":"<&>"}`, &five, false},
		{`{"reply_to":"x","a":1}`, `{"a":1}`, nil, false},
		{`[1]`, "", nil, true},
		{`"x"`, "", nil, true},
		{`{`, "", nil, true},
	} {
		wire, reply, err := SplitMetadata([]byte(c.in))
		if (err != nil) != c.bad {
			t.Errorf("%q: err %v, want bad=%v", c.in, err, c.bad)
			continue
		}
		if string(wire) != c.wire || (reply == nil) != (c.reply == nil) || reply != nil && *reply != *c.reply {
			t.Errorf("%q: %q %v, want %q %v", c.in, wire, reply, c.wire, c.reply)
		}
	}
	// The stored form of a reply (reply_to merged in, keys re-encoded)
	// gives back the same wire bytes as the proposal.
	a, _, _ := SplitMetadata([]byte(`{"k": [1, 2], "a": "x"}`))
	b, _, _ := SplitMetadata([]byte(`{"a":"x","k":[1,2],"reply_to":9}`))
	if string(a) != string(b) {
		t.Errorf("wire forms differ: %s %s", a, b)
	}
}

// A replica's changes are never logged: only its host keeps the log.
func TestLog_NotForReplicas(t *testing.T) {
	h := newHostRoom(t, "R", 1)
	join(t, h.db, "R", "c1@c.example", "read", "a.example")
	n := len(events(t, h.db, "R"))
	if _, err := h.db.Exec(`UPDATE rooms SET home_domain = 'c.example' WHERE id = 'R'`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	inTx(t, h.db, func(tx *sql.Tx) error {
		if err := LogMessage(ctx, tx, "R", h.firstMsg, "a.example", 256<<10); err != nil {
			return err
		}
		return LogRoom(ctx, tx, "R", "a.example")
	})
	if got := len(events(t, h.db, "R")); got != n {
		t.Errorf("replica logged %d events", got-n)
	}
}

// Payloads keep <, > and & unescaped, so events stay near the size of
// the message they carry.
func TestLog_PayloadNotHTMLEscaped(t *testing.T) {
	h := newHostRoom(t, "R", 0)
	if _, err := h.db.Exec(`UPDATE rooms SET name = 'a<b>&c' WHERE id = 'R'`); err != nil {
		t.Fatal(err)
	}
	join(t, h.db, "R", "c1@c.example", "read", "a.example")
	if got := events(t, h.db, "R"); string(got[0].Payload) != `{"name":"a<b>&c","archived":false}` {
		t.Errorf("room payload: %s", got[0].Payload)
	}
}

// A message whose event would not fit in what member servers read, 64
// KiB plus twice the message cap with the push around it, is refused
// and not logged.
func TestLogMessage_EventTooLarge(t *testing.T) {
	h := newHostRoom(t, "R", 0)
	join(t, h.db, "R", "c1@c.example", "read", "a.example")
	n := len(events(t, h.db, "R"))
	// Control characters take 6 bytes each in JSON.
	id := addKey(t, h.db, `INSERT INTO messages(room_id, author_key_id, ts, content, content_type, message_type, trust_origin)
		VALUES ('R', ?, '2026-01-04T00:00:00.000000000Z', ?, 'text/plain', 'answer', 'internal')`, h.bot, strings.Repeat("\x01", 12<<10))
	ctx := context.Background()
	tx, err := h.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := LogMessage(ctx, tx, "R", id, "a.example", 1<<10); !errors.Is(err, ErrEventTooLarge) {
		t.Errorf("1 KiB cap: %v, want ErrEventTooLarge", err)
	}
	_ = tx.Rollback()
	if got := len(events(t, h.db, "R")); got != n {
		t.Errorf("logged %d events", got-n)
	}
	inTx(t, h.db, func(tx *sql.Tx) error { return LogMessage(ctx, tx, "R", id, "a.example", 256<<10) })
	if got := len(events(t, h.db, "R")); got != n+1 {
		t.Errorf("256 KiB cap: logged %d events, want 1", got-n)
	}
}
