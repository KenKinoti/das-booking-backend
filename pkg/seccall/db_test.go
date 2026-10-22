package seccall

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

func TestRoomLifecycleAndTokens(t *testing.T) {
	db := testDB(t)
	c := newClock()
	s := svcAt(db, c)
	l := newLink(t)

	// who may create
	for _, role := range []string{"staff", "read_only", "bartender", ""} {
		_, err := s.Create(orgA, hostA, role, "X", CreateInput{LinkCheck: l.check})
		mustCode(t, err, "FORBIDDEN")
	}
	for _, role := range []string{"owner", "admin", "manager", "super_admin", "org_admin"} {
		if _, err := s.Create(orgA, "u-"+role, role, "X", CreateInput{LinkCheck: l.check}); err != nil {
			t.Fatalf("%s: %v", role, err)
		}
	}
	_, err := s.Create(orgA, hostA, "manager", "X", CreateInput{})
	mustCode(t, err, "INVALID") // no link check
	_, err = s.Create(orgA, hostA, "manager", "X", CreateInput{LinkCheck: l.check, Expires: "3d"})
	mustCode(t, err, "INVALID")

	v := mkRoom(t, s, l, CreateInput{Title: "Contract review", Expires: "1h"})
	if !ValidRoomID(v.ID) || v.Status != "active" || v.Path != "/c/"+v.ID || v.Lobby || v.MaxParticipants != 6 {
		t.Fatalf("room %+v", v)
	}
	if d := v.ExpiresAt.Sub(v.CreatedAt); d != time.Hour {
		t.Fatalf("expiry %s", d)
	}
	var stored Room
	db.First(&stored, "id = ?", v.ID)
	if stored.CheckHash == l.check || len(stored.CheckHash) != 64 {
		t.Fatal("link check must be stored hashed")
	}

	// preview and wrong links
	_, err = s.Preview(v.ID, newLink(t).check, "198.51.100.1")
	mustCode(t, err, "WRONG_LINK")
	_, err = s.Preview(NewRoomID(), l.check, "198.51.100.1")
	mustCode(t, err, "NOT_FOUND")
	pr, err := s.Preview(v.ID, l.check, "198.51.100.1")
	if err != nil || pr.Title != "Contract review" || pr.HostName != "Kennedy K" {
		t.Fatalf("preview %+v %v", pr, err)
	}
	_, err = s.JoinGuest(v.ID, JoinInput{Name: "Mallory", LinkCheck: newLink(t).check}, "198.51.100.66")
	mustCode(t, err, "WRONG_LINK")
	_, err = s.JoinGuest(v.ID, JoinInput{Name: "Mallory"}, "198.51.100.66")
	mustCode(t, err, "WRONG_LINK")
	_, err = s.JoinGuest(v.ID, JoinInput{Name: "  ", LinkCheck: l.check}, "198.51.100.66")
	mustCode(t, err, "INVALID")

	// host + two guests
	host, err := s.JoinHost(orgA, hostA, v.ID, JoinInput{Name: "Kennedy", LinkCheck: l.check})
	if err != nil || !host.IsHost || host.State != StateActive || len(host.Peers) != 0 {
		t.Fatalf("host join %+v %v", host, err)
	}
	g1 := join(t, s, v.ID, l, "Amina\u202e\u2067\u200b\x07 Otieno", "198.51.100.2")
	if g1.State != StateActive || len(g1.Peers) != 1 || g1.Peers[0].ID != host.ParticipantID || !g1.Peers[0].IsHost {
		t.Fatalf("g1 %+v", g1)
	}
	g2 := join(t, s, v.ID, l, "Brian", "198.51.100.3")
	if len(g2.Peers) != 2 {
		t.Fatalf("g2 peers %+v", g2.Peers)
	}

	// tokens are hashed: no raw token anywhere in the table
	var rows []map[string]any
	db.Raw(`SELECT * FROM seccall_participants`).Scan(&rows)
	dump, _ := json.Marshal(rows)
	for _, tok := range []string{host.Token, g1.Token, g2.Token} {
		if strings.Contains(string(dump), tok) || strings.Contains(string(dump), strings.TrimPrefix(tok, tokenPrefix)) {
			t.Fatal("raw participant token stored")
		}
	}
	var p Participant
	db.First(&p, "id = ?", g1.ParticipantID)
	if p.TokenHash != HashToken(g1.Token) || p.Name != "Amina Otieno" {
		t.Fatalf("participant %+v", p)
	}

	// host sees two joins, in order; guest 1 sees guest 2's join only
	hp := pollNow(t, s, v.ID, host.Token, host.Since)
	if ev := events(hp); strings.Join(ev, ",") != "join,join" || len(hp.Peers) != 2 {
		t.Fatalf("host events %v peers %d", ev, len(hp.Peers))
	}
	gp := pollNow(t, s, v.ID, g1.Token, g1.Since)
	if ev := events(gp); strings.Join(ev, ",") != "join" {
		t.Fatalf("g1 events %v", ev)
	}

	// signal: direct + broadcast; the relay hands back exactly what was sent
	aad := func(from, to string) string { return "dasyin-seccall/v1|" + v.ID + "|" + from + "|" + to }
	direct := l.seal(`{"n":1,"t":"offer","sdp":"v=0"}`, aad(g1.ParticipantID, host.ParticipantID))
	bcast := l.seal(`{"n":2,"t":"state","mic":true}`, aad(g1.ParticipantID, "*"))
	res, err := s.Signal(v.ID, g1.Token, []SignalIn{{To: host.ParticipantID, Ciphertext: direct}, {To: "*", Ciphertext: bcast}, {To: "nobody", Ciphertext: bcast}})
	if err != nil || res.Queued != 2 || res.Dropped != 1 {
		t.Fatalf("signal %+v %v", res, err)
	}
	hp2 := pollNow(t, s, v.ID, host.Token, hp.Next)
	if len(hp2.Messages) != 2 || hp2.Messages[0].Ciphertext != direct || hp2.Messages[0].To != host.ParticipantID ||
		hp2.Messages[1].Ciphertext != bcast || hp2.Messages[1].To != "*" || hp2.Messages[0].From != g1.ParticipantID {
		t.Fatalf("host got %+v", hp2.Messages)
	}
	if pt, err := l.open(hp2.Messages[0].Ciphertext, aad(hp2.Messages[0].From, hp2.Messages[0].To)); err != nil || !strings.Contains(pt, "offer") {
		t.Fatal("ciphertext changed in transit")
	}
	g2p := pollNow(t, s, v.ID, g2.Token, g2.Since)
	if len(g2p.Messages) != 1 || g2p.Messages[0].Ciphertext != bcast {
		t.Fatalf("g2 should get only the broadcast: %+v", g2p.Messages)
	}
	g1p := pollNow(t, s, v.ID, g1.Token, gp.Next)
	if len(g1p.Messages) != 0 {
		t.Fatalf("sender got its own messages: %+v", g1p.Messages)
	}

	// relabelled sender → authentication fails (what a tampering relay would get)
	if _, err := l.open(direct, aad(g2.ParticipantID, host.ParticipantID)); err == nil {
		t.Fatal("AAD did not bind the sender")
	}

	// leave: token stops working, others get "leave"
	if err := s.Leave(v.ID, g2.Token); err != nil {
		t.Fatal(err)
	}
	_, err = s.Signal(v.ID, g2.Token, []SignalIn{{To: "*", Ciphertext: bcast}})
	mustCode(t, err, "NOT_IN_CALL")
	_, err = s.Poll(t.Context(), v.ID, g2.Token, 0, time.Millisecond, "198.51.100.3")
	mustCode(t, err, "NOT_IN_CALL")
	hp3 := pollNow(t, s, v.ID, host.Token, hp2.Next)
	if ev := events(hp3); strings.Join(ev, ",") != "leave" || len(hp3.Peers) != 1 {
		t.Fatalf("after leave %v %d", ev, len(hp3.Peers))
	}

	// host list
	list, _ := s.List(orgA, hostA, "active")
	if len(list) != 1 || list[0].ID != v.ID || list[0].Participants != 2 {
		t.Fatalf("list %+v", list)
	}
	det, _ := s.Get(orgA, hostA, v.ID)
	if len(det.People) != 3 {
		t.Fatalf("people %d", len(det.People))
	}

	// end for all → everybody gets "This call has ended"; late joiners too
	if err := s.End(orgA, hostA, v.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.Poll(t.Context(), v.ID, g1.Token, 0, time.Millisecond, "198.51.100.2")
	mustCode(t, err, "CALL_ENDED")
	_, err = s.JoinGuest(v.ID, JoinInput{Name: "Late", LinkCheck: l.check}, "198.51.100.9")
	mustCode(t, err, "CALL_ENDED")
	_, err = s.JoinHost(orgA, hostA, v.ID, JoinInput{Name: "K", LinkCheck: l.check})
	mustCode(t, err, "CALL_ENDED")
	var n int64
	db.Model(&Message{}).Where("room_id = ?", v.ID).Count(&n)
	if n != 0 {
		t.Fatalf("%d relayed messages left after end", n)
	}
	ended, _ := s.List(orgA, hostA, "ended")
	if len(ended) != 1 || ended[0].EndReason != "host" || ended[0].PeakParticipants != 3 {
		t.Fatalf("ended %+v", ended)
	}
}

func TestExpiryLockLobbyRemove(t *testing.T) {
	db := testDB(t)
	c := newClock()
	s := svcAt(db, c)
	l := newLink(t)

	// expiry
	v := mkRoom(t, s, l, CreateInput{Expires: "1h"})
	g := join(t, s, v.ID, l, "Amina", "198.51.100.2")
	c.add(61 * time.Minute)
	_, err := s.JoinGuest(v.ID, JoinInput{Name: "Late", LinkCheck: l.check}, "198.51.100.3")
	mustCode(t, err, "CALL_ENDED")
	_, err = s.Poll(t.Context(), v.ID, g.Token, 0, time.Millisecond, "x")
	mustCode(t, err, "CALL_ENDED")
	r := s.Cleanup(t.Context())
	if r.Expired != 1 {
		t.Fatalf("cleanup %+v", r)
	}
	var room Room
	db.First(&room, "id = ?", v.ID)
	if room.EndedAt == nil || room.EndReason != "expired" {
		t.Fatalf("not ended: %+v", room)
	}

	// lock
	v = mkRoom(t, s, l, CreateInput{})
	host, _ := s.JoinHost(orgA, hostA, v.ID, JoinInput{Name: "K", LinkCheck: l.check})
	g1 := join(t, s, v.ID, l, "Amina", "198.51.100.2")
	if err := s.SetLocked(orgA, hostA, v.ID, true); err != nil {
		t.Fatal(err)
	}
	_, err = s.JoinGuest(v.ID, JoinInput{Name: "New", LinkCheck: l.check}, "198.51.100.4")
	mustCode(t, err, "LOCKED")
	if _, err := s.JoinHost(orgA, hostA, v.ID, JoinInput{Name: "K phone", LinkCheck: l.check}); err != nil {
		t.Fatalf("host refused by own lock: %v", err)
	}
	gp := pollNow(t, s, v.ID, g1.Token, g1.Since)
	if !gp.Room.Locked || !strings.Contains(strings.Join(events(gp), ","), "lock") {
		t.Fatalf("lock not announced: %v", events(gp))
	}
	s.SetLocked(orgA, hostA, v.ID, false)
	join(t, s, v.ID, l, "New", "198.51.100.4")

	// remove
	if err := s.Remove(orgA, hostA, v.ID, g1.ParticipantID); err != nil {
		t.Fatal(err)
	}
	_, err = s.Poll(t.Context(), v.ID, g1.Token, 0, time.Millisecond, "x")
	mustCode(t, err, "REMOVED")
	_, err = s.Signal(v.ID, g1.Token, []SignalIn{{To: "*", Ciphertext: l.seal("hello there, this is long enough", "a")}})
	mustCode(t, err, "REMOVED")
	hp := pollNow(t, s, v.ID, host.Token, host.Since)
	found := false
	for _, m := range hp.Messages {
		if m.Kind == "evt" && strings.Contains(string(m.Event), `"removed"`) && strings.Contains(string(m.Event), g1.ParticipantID) {
			found = true
		}
	}
	if !found {
		t.Fatalf("others not told about the removal: %+v", hp.Messages)
	}
	err = s.Remove(orgA, hostA, v.ID, host.ParticipantID)
	mustCode(t, err, "INVALID")

	// lobby
	v = mkRoom(t, s, l, CreateInput{Lobby: true})
	host, _ = s.JoinHost(orgA, hostA, v.ID, JoinInput{Name: "K", LinkCheck: l.check})
	if host.State != StateActive {
		t.Fatal("host waited in own lobby")
	}
	w := join(t, s, v.ID, l, "Waiting Wanjiru", "198.51.100.5")
	if w.State != StateLobby || len(w.Peers) != 0 {
		t.Fatalf("lobby join %+v", w)
	}
	_, err = s.Signal(v.ID, w.Token, []SignalIn{{To: "*", Ciphertext: l.seal("hello there, this is long enough", "a")}})
	mustCode(t, err, "NOT_ADMITTED")
	_, err = s.ICE(v.ID, w.Token)
	mustCode(t, err, "NOT_ADMITTED")
	hp = pollNow(t, s, v.ID, host.Token, host.Since)
	if ev := events(hp); len(ev) != 1 || ev[0] != "knock" || len(hp.Lobby) != 1 || hp.Lobby[0].Name != "Waiting Wanjiru" {
		t.Fatalf("host lobby %v %+v", ev, hp.Lobby)
	}
	// a waiting guest receives nothing meant for the call
	s.Signal(v.ID, host.Token, []SignalIn{{To: "*", Ciphertext: l.seal("broadcast to the call, long enough", "a")}})
	wp := pollNow(t, s, v.ID, w.Token, w.Since)
	if len(wp.Messages) != 0 || wp.You.State != StateLobby {
		t.Fatalf("lobby guest got %+v", wp.Messages)
	}
	if err := s.Admit(orgA, hostA, v.ID, w.ParticipantID); err != nil {
		t.Fatal(err)
	}
	wp = pollNow(t, s, v.ID, w.Token, wp.Next)
	if wp.You.State != StateActive || len(wp.Peers) != 1 || events(wp)[0] != "admitted" {
		t.Fatalf("after admit %+v %v", wp.You, events(wp))
	}
	d := join(t, s, v.ID, l, "Denied Dan", "198.51.100.6")
	if err := s.Deny(orgA, hostA, v.ID, d.ParticipantID); err != nil {
		t.Fatal(err)
	}
	_, err = s.Poll(t.Context(), v.ID, d.Token, 0, time.Millisecond, "x")
	mustCode(t, err, "DENIED")
	err = s.Admit(orgA, hostA, v.ID, d.ParticipantID)
	mustCode(t, err, "NOT_WAITING")
	// turning the lobby off lets everyone waiting in
	e := join(t, s, v.ID, l, "Esther", "198.51.100.7")
	if e.State != StateLobby {
		t.Fatal("expected lobby")
	}
	if err := s.Update(orgA, hostA, v.ID, UpdateInput{Lobby: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	ep := pollNow(t, s, v.ID, e.Token, e.Since)
	if ep.You.State != StateActive {
		t.Fatalf("not let in when the lobby was turned off: %+v", ep.You)
	}
}

func ptr[T any](v T) *T { return &v }

func TestLimits(t *testing.T) {
	db := testDB(t)
	c := newClock()
	s := svcAt(db, c)
	l := newLink(t)

	// room size
	v := mkRoom(t, s, l, CreateInput{MaxParticipants: 3})
	for i := 0; i < 3; i++ {
		join(t, s, v.ID, l, fmt.Sprintf("P%d", i), fmt.Sprintf("198.51.100.%d", 10+i))
	}
	_, err := s.JoinGuest(v.ID, JoinInput{Name: "Fourth", LinkCheck: l.check}, "198.51.100.20")
	mustCode(t, err, "FULL")
	_, err = s.Create(orgA, hostA, "manager", "K", CreateInput{LinkCheck: l.check, MaxParticipants: 7})
	mustCode(t, err, "INVALID")

	// per-IP join attempts (in memory)
	s.Limits.JoinPerIPPerMin = 3
	v2 := mkRoom(t, s, l, CreateInput{})
	for i := 0; i < 3; i++ {
		s.JoinGuest(v2.ID, JoinInput{Name: "x", LinkCheck: newLink(t).check}, "192.0.2.1")
	}
	_, err = s.JoinGuest(v2.ID, JoinInput{Name: "x", LinkCheck: l.check}, "192.0.2.1")
	mustCode(t, err, "RATE_LIMITED")
	if _, err := s.JoinGuest(v2.ID, JoinInput{Name: "y", LinkCheck: l.check}, "192.0.2.2"); err != nil {
		t.Fatalf("other IP limited: %v", err)
	}
	s.Limits.JoinPerIPPerMin = 100

	// per-room join attempts (database: holds across instances)
	s.Limits.JoinsPerRoomPer10m = 4
	v3 := mkRoom(t, s, l, CreateInput{})
	var last *JoinResult
	for i := 0; i < 4; i++ {
		last = join(t, s, v3.ID, l, "j", fmt.Sprintf("192.0.2.%d", 30+i))
		s.Leave(v3.ID, last.Token)
	}
	other := New(db) // a second instance: its own memory, same database
	other.Now = c.now
	other.Limits = s.Limits
	_, err = other.JoinGuest(v3.ID, JoinInput{Name: "j", LinkCheck: l.check}, "192.0.2.99")
	mustCode(t, err, "RATE_LIMITED")
	c.add(11 * time.Minute)
	if _, err := other.JoinGuest(v3.ID, JoinInput{Name: "j", LinkCheck: l.check}, "192.0.2.99"); err != nil {
		t.Fatalf("room limit did not reset: %v", err)
	}
	s.Limits.JoinsPerRoomPer10m = 40

	// signalling: batch size, per participant, per room
	v4 := mkRoom(t, s, l, CreateInput{})
	a := join(t, s, v4.ID, l, "A", "192.0.2.50")
	b := join(t, s, v4.ID, l, "B", "192.0.2.51")
	msg := func() SignalIn {
		return SignalIn{To: b.ParticipantID, Ciphertext: l.seal("candidate:1 1 udp 2122260223 192.168.1.2 5000 typ host", "x")}
	}
	batch := make([]SignalIn, s.Limits.BatchMax+1)
	for i := range batch {
		batch[i] = msg()
	}
	_, err = s.Signal(v4.ID, a.Token, batch)
	mustCode(t, err, "INVALID")
	s.Limits.SignalPerPartPerMin = 5
	for i := 0; i < 5; i++ {
		if _, err := s.Signal(v4.ID, a.Token, []SignalIn{msg()}); err != nil {
			t.Fatal(err)
		}
	}
	_, err = s.Signal(v4.ID, a.Token, []SignalIn{msg()})
	mustCode(t, err, "RATE_LIMITED")
	s.Limits.SignalPerPartPerMin = 1000
	s.Limits.RoomMsgsPerMin = 9 // 2 join events + 5 signals already this minute
	if _, err := s.Signal(v4.ID, b.Token, []SignalIn{{To: a.ParticipantID, Ciphertext: msg().Ciphertext}, {To: a.ParticipantID, Ciphertext: msg().Ciphertext}}); err != nil {
		t.Fatal(err)
	}
	other.Limits = s.Limits // the per-room count lives in the database: another instance sees it too
	_, err = other.Signal(v4.ID, b.Token, []SignalIn{{To: a.ParticipantID, Ciphertext: msg().Ciphertext}, {To: a.ParticipantID, Ciphertext: msg().Ciphertext}})
	mustCode(t, err, "RATE_LIMITED")

	// creates per user per hour
	s.Limits.CreatePerUserPerHr = 1
	if _, err := s.Create(orgA, "u-rate", "manager", "x", CreateInput{LinkCheck: l.check}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Create(orgA, "u-rate", "manager", "x", CreateInput{LinkCheck: l.check})
	mustCode(t, err, "RATE_LIMITED")
}

func TestCrossOrgIsolation(t *testing.T) {
	db := testDB(t)
	s := svcAt(db, newClock())
	l := newLink(t)
	v := mkRoom(t, s, l, CreateInput{Lobby: true})
	w := join(t, s, v.ID, l, "W", "192.0.2.1")
	type actor struct{ org, user string }
	for _, a := range []actor{{orgB, "user-b"}, {orgB, hostA}, {orgA, "another-manager-a"}} {
		_, err := s.Get(a.org, a.user, v.ID)
		mustCode(t, err, "NOT_FOUND")
		mustCode(t, s.End(a.org, a.user, v.ID), "NOT_FOUND")
		mustCode(t, s.SetLocked(a.org, a.user, v.ID, true), "NOT_FOUND")
		mustCode(t, s.Admit(a.org, a.user, v.ID, w.ParticipantID), "NOT_FOUND")
		mustCode(t, s.Remove(a.org, a.user, v.ID, w.ParticipantID), "NOT_FOUND")
		_, err = s.JoinHost(a.org, a.user, v.ID, JoinInput{Name: "x", LinkCheck: l.check})
		mustCode(t, err, "NOT_FOUND")
		list, _ := s.List(a.org, a.user, "")
		if len(list) != 0 {
			t.Fatalf("%v sees %d calls", a, len(list))
		}
	}
	// a participant token of one room is useless in another
	v2 := mkRoom(t, s, l, CreateInput{})
	_, err := s.Poll(t.Context(), v2.ID, w.Token, 0, time.Millisecond, "x")
	mustCode(t, err, "NOT_IN_CALL")
	var r Room
	db.First(&r, "id = ?", v.ID)
	if r.Locked || r.EndedAt != nil {
		t.Fatal("another organisation changed the room")
	}
}

func TestRelayOrderingAcrossInstances(t *testing.T) {
	db := testDB(t)
	c := newClock()
	a := svcAt(db, c)
	// a second "instance": its own connection pool, hub, limiter and listener
	db2 := openDB(t, testDSN2(t))
	b := New(db2)
	b.Now = c.now
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.StartListener(ctx)
	b.StartListener(ctx)
	for i := 0; i < 50 && !(a.listenerUp() && b.listenerUp()); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if !a.listenerUp() || !b.listenerUp() {
		t.Fatal("listeners did not start")
	}
	l := newLink(t)
	v := mkRoom(t, a, l, CreateInput{})
	recv := join(t, b, v.ID, l, "Receiver", "192.0.2.1")
	const senders, each = 4, 25
	sp := make([]*JoinResult, senders)
	for i := range sp {
		sp[i] = join(t, a, v.ID, l, fmt.Sprintf("S%d", i), fmt.Sprintf("192.0.2.%d", 10+i))
	}
	since := recv.Since
	// drain the joins
	since = pollNow(t, b, v.ID, recv.Token, since).Next

	// long poll on instance B wakes for a message sent through instance A
	got := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		res, err := b.Poll(context.Background(), v.ID, recv.Token, since, 10*time.Second, "192.0.2.1")
		if err == nil && len(res.Messages) > 0 {
			got <- time.Since(start)
		} else {
			got <- -1
		}
	}()
	time.Sleep(300 * time.Millisecond)
	if _, err := a.Signal(v.ID, sp[0].Token, []SignalIn{{To: recv.ParticipantID, Ciphertext: l.seal(`{"n":0,"t":"ping","pad":"................"}`, "x")}}); err != nil {
		t.Fatal(err)
	}
	if d := <-got; d < 0 || d > 2500*time.Millisecond {
		t.Fatalf("cross-instance wake-up took %s", d)
	}

	// concurrent senders on both instances: every message arrives once, in
	// increasing sequence order, and each sender's messages keep their order
	var wg sync.WaitGroup
	for i := range sp {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			inst := a
			if i%2 == 1 {
				inst = b
			}
			for n := 1; n <= each; n++ {
				ct := l.seal(fmt.Sprintf(`{"n":%d,"s":%d,"t":"cand","c":"candidate:1 1 udp 2122260223 10.0.0.%d 5000 typ host"}`, n, i, n), "x")
				if _, err := inst.Signal(v.ID, sp[i].Token, []SignalIn{{To: recv.ParticipantID, Ciphertext: ct}}); err != nil {
					t.Error(err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	cur := since
	lastSeq := int64(0)
	perSender := map[string]int{}
	total := 0
	for i := 0; i < 50 && total < senders*each+1; i++ {
		res := pollNow(t, b, v.ID, recv.Token, cur)
		for _, m := range res.Messages {
			if m.Seq <= lastSeq {
				t.Fatalf("out of order: %d after %d", m.Seq, lastSeq)
			}
			lastSeq = m.Seq
			if m.Kind != "sig" {
				continue
			}
			pt, err := l.open(m.Ciphertext, "x")
			if err != nil {
				t.Fatal("ciphertext altered")
			}
			var body struct{ N int }
			json.Unmarshal([]byte(pt), &body)
			if body.N > 0 {
				if body.N != perSender[m.From]+1 {
					t.Fatalf("sender %s: message %d after %d", m.From, body.N, perSender[m.From])
				}
				perSender[m.From] = body.N
			}
			total++
		}
		cur = res.Next
	}
	if total != senders*each+1 {
		t.Fatalf("received %d of %d", total, senders*each+1)
	}
}

func testDSN2(t *testing.T) string {
	// same schema as testDB (already created there)
	dsn := testDSN(t, "seccall_unused")
	return strings.Replace(dsn, "search_path=seccall_unused", "search_path=seccall_test", 1)
}

func TestCleanupStaleResumeAndPurge(t *testing.T) {
	db := testDB(t)
	c := newClock()
	s := svcAt(db, c)
	l := newLink(t)
	v := mkRoom(t, s, l, CreateInput{Expires: "7d"})
	host, _ := s.JoinHost(orgA, hostA, v.ID, JoinInput{Name: "K", LinkCheck: l.check})
	g := join(t, s, v.ID, l, "Quiet Quentin", "192.0.2.1")
	s.Signal(v.ID, g.Token, []SignalIn{{To: "*", Ciphertext: l.seal("some encrypted hello message here", "x")}})

	// 50 s without a poll from the guest; the host keeps polling
	c.add(50 * time.Second)
	hp := pollNow(t, s, v.ID, host.Token, host.Since)
	r := s.Cleanup(t.Context())
	if r.Stale != 1 {
		t.Fatalf("cleanup %+v", r)
	}
	var p Participant
	db.First(&p, "id = ?", g.ParticipantID)
	if p.State != StateLeft || p.LeftReason != "stale" {
		t.Fatalf("participant %+v", p)
	}
	hp = pollNow(t, s, v.ID, host.Token, hp.Next)
	if ev := events(hp); len(ev) != 1 || ev[0] != "leave" || len(hp.Peers) != 0 {
		t.Fatalf("host after stale %v %d", ev, len(hp.Peers))
	}
	// the guest comes back (network blip): same participant, back in the call
	gp := pollNow(t, s, v.ID, g.Token, g.Since)
	if gp.You.State != StateActive || gp.You.ID != g.ParticipantID {
		t.Fatalf("resume %+v", gp.You)
	}
	hp = pollNow(t, s, v.ID, host.Token, hp.Next)
	if ev := events(hp); len(ev) != 1 || ev[0] != "join" {
		t.Fatalf("host after resume %v", ev)
	}

	// relayed messages are removed after two minutes
	c.add(3 * time.Minute)
	pollNow(t, s, v.ID, host.Token, hp.Next)
	pollNow(t, s, v.ID, g.Token, gp.Next)
	r = s.Cleanup(t.Context())
	if r.Messages == 0 {
		t.Fatalf("old messages not deleted: %+v", r)
	}
	var n int64
	db.Model(&Message{}).Where("created_at < ?", c.now().Add(-2*time.Minute)).Count(&n)
	if n != 0 {
		t.Fatal("messages older than 2 minutes remain")
	}

	// ended rooms are forgotten after KeepEnded
	s.End(orgA, hostA, v.ID)
	c.add(s.Limits.KeepEnded + time.Hour)
	r = s.Cleanup(t.Context())
	if r.Purged != 1 {
		t.Fatalf("purge %+v", r)
	}
	db.Model(&Participant{}).Where("room_id = ?", v.ID).Count(&n)
	if n != 0 {
		t.Fatal("participants of purged room remain")
	}

	// one instance at a time
	other := openDB(t, testDSN2(t))
	sqlDB, _ := other.DB()
	conn, err := sqlDB.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(t.Context(), "SELECT pg_advisory_lock(hashtext('seccall:cleanup'))"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.CleanupLocked(t.Context()); ok {
		t.Fatal("cleanup ran while another instance held the lock")
	}
	conn.ExecContext(t.Context(), "SELECT pg_advisory_unlock(hashtext('seccall:cleanup'))")
	if _, ok := s.CleanupLocked(t.Context()); !ok {
		t.Fatal("cleanup did not run after the lock was released")
	}
}

func TestICEServers(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "test-encryption-key-for-seccall")
	db := testDB(t)
	c := newClock()
	s := svcAt(db, c)
	l := newLink(t)
	v := mkRoom(t, s, l, CreateInput{Expires: "24h"})
	g := join(t, s, v.ID, l, "G", "192.0.2.1")

	ice, err := s.ICE(v.ID, g.Token)
	if err != nil || ice.Relay || len(ice.ICEServers) != 1 || ice.ICEServers[0].URLs[0] != DefaultSTUN[0] {
		t.Fatalf("default %+v %v", ice, err)
	}
	_, err = s.ICE(v.ID, "sct_nope-nope-nope-nope")
	mustCode(t, err, "NOT_IN_CALL")

	// validation
	_, err = s.SaveSettings(SettingsInput{Mode: ModeStatic, TURNURLs: []string{"turn:turn.example.com:3478"}}, "k")
	mustCode(t, err, "INVALID")
	_, err = s.SaveSettings(SettingsInput{Mode: ModeCoturn, TURNURLs: []string{"http://x"}, SharedSecret: "0123456789abcdef0123"}, "k")
	mustCode(t, err, "INVALID")

	// static: password encrypted at rest, never returned
	sv, err := s.SaveSettings(SettingsInput{Mode: ModeStatic, TURNURLs: []string{"turn:turn.example.com:3478?transport=udp, turns:turn.example.com:5349"}, Username: "dasyin", Credential: "very-secret-turn-password"}, "kennedy")
	if err != nil || !sv.CredentialSet || sv.CredentialHnt != "••••rd" || len(sv.TURNURLs) != 2 {
		t.Fatalf("static %+v %v", sv, err)
	}
	var st Settings
	db.First(&st, "id = ?", "platform")
	if strings.Contains(st.Credential, "very-secret") || !mail.IsEncrypted(st.Credential) {
		t.Fatal("TURN password stored in plain text")
	}
	b, _ := json.Marshal(s.SettingsFor())
	if strings.Contains(string(b), "very-secret") {
		t.Fatal("password returned to the settings page")
	}
	ice, _ = s.ICE(v.ID, g.Token)
	if !ice.Relay || len(ice.ICEServers) != 2 || ice.ICEServers[1].Credential != "very-secret-turn-password" || ice.ICEServers[1].Username != "dasyin" {
		t.Fatalf("static ice %+v", ice)
	}

	// coturn shared secret: per participant, valid until the call expires
	_, err = s.SaveSettings(SettingsInput{Mode: ModeCoturn, TURNURLs: []string{"turn:turn.example.com:3478"}, SharedSecret: "turn-shared-secret-0123456789"}, "kennedy")
	if err != nil {
		t.Fatal(err)
	}
	db.First(&st, "id = ?", "platform")
	if strings.Contains(st.SharedSecret, "turn-shared-secret") {
		t.Fatal("shared secret stored in plain text")
	}
	ice, _ = s.ICE(v.ID, g.Token)
	var room Room
	db.First(&room, "id = ?", v.ID)
	wantU, wantC := CoturnCredential("turn-shared-secret-0123456789", g.ParticipantID, room.ExpiresAt)
	if !ice.Relay || ice.ICEServers[1].Username != wantU || ice.ICEServers[1].Credential != wantC || !ice.ExpiresAt.Equal(room.ExpiresAt) {
		t.Fatalf("coturn ice %+v", ice)
	}
	if !strings.HasPrefix(wantU, fmt.Sprint(room.ExpiresAt.Unix())+":") {
		t.Fatal("TTL is not the call expiry")
	}
	g2 := join(t, s, v.ID, l, "G2", "192.0.2.2")
	ice2, _ := s.ICE(v.ID, g2.Token)
	if ice2.ICEServers[1].Username == ice.ICEServers[1].Username {
		t.Fatal("participants share a TURN username")
	}
	// keeping the secret when saving other fields; clearing it
	if _, err := s.SaveSettings(SettingsInput{Mode: ModeCoturn, TURNURLs: []string{"turn:turn2.example.com:3478"}}, "k"); err != nil {
		t.Fatalf("secret not kept: %v", err)
	}
	if _, err := s.SaveSettings(SettingsInput{Mode: ModeSTUN, STUNURLs: []string{"stun:stun.example.com:3478"}, ClearSecret: true, ClearCredential: true}, "k"); err != nil {
		t.Fatal(err)
	}
	ice, _ = s.ICE(v.ID, g.Token)
	if ice.Relay || ice.ICEServers[0].URLs[0] != "stun:stun.example.com:3478" {
		t.Fatalf("stun only %+v", ice)
	}
}
