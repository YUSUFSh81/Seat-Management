package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/YUSUFSh81/Seat-Management/internal/auth"
)

var run = fmt.Sprintf("%x", time.Now().UnixNano()&0xffffffff)

func uid(s string) string { return run + "-" + s }

var (
	base   string
	secret []byte
	client *http.Client

	tokMu sync.Mutex
	toks  = map[string]string{}

	overall = newTally()
	phases  []phase
	latMu   sync.Mutex
	lats    []time.Duration

	newRes, newSeats, netSeats, cancelledRes atomic.Int64 // what this run expects the server to hold
	dupViol, seqViol                         atomic.Int64

	checks []check
)

type phase struct {
	name string
	t    *tally
}
type check struct {
	name, detail string
	ok           bool
}

func expect(name string, ok bool, f string, a ...any) {
	checks = append(checks, check{name: name, ok: ok, detail: fmt.Sprintf(f, a...)})
}

// ---------- tally ----------
type tally struct {
	mu sync.Mutex
	m  map[string]int
}

func newTally() *tally { return &tally{m: map[string]int{}} }
func (t *tally) add(k string) {
	t.mu.Lock()
	t.m[k]++
	t.mu.Unlock()
}
func (t *tally) get(k string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.m[k]
}
func (t *tally) total() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, v := range t.m {
		n += v
	}
	return n
}

// ---------- http ----------
type resp struct {
	status int
	body   []byte
	hdr    http.Header
	err    error
	dur    time.Duration
}

func token(user, role string) string {
	tokMu.Lock()
	defer tokMu.Unlock()
	k := user + "|" + role
	if t, ok := toks[k]; ok {
		return t
	}
	t, err := auth.Sign(secret, user, role, 3*time.Hour)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	toks[k] = t
	return t
}

func call(method, path, tok string, payload any, hdr map[string]string) resp {
	var rd io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		return resp{err: err}
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	start := time.Now()
	r, err := client.Do(req)
	if err != nil {
		return resp{err: err, dur: time.Since(start)}
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return resp{status: r.StatusCode, body: b, hdr: r.Header, dur: time.Since(start)}
}

// classify keys off the status and a substring of the error text, so it survives wording changes.
func classify(r resp) string {
	switch {
	case r.err != nil:
		return "network_error"
	case r.status == 201:
		if r.hdr.Get("Idempotent-Replay") == "true" {
			return "201_replay"
		}
		return "201_confirmed"
	case r.status == 409:
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(r.body, &e)
		s := strings.ToLower(e.Error)
		switch {
		case strings.Contains(s, "unavailable"):
			return "409_seat_taken"
		case strings.Contains(s, "limit"):
			return "409_over_limit"
		case strings.Contains(s, "idempotency"):
			return "409_key_conflict"
		}
		return "409_other"
	case r.status >= 500:
		return "5xx"
	}
	return fmt.Sprintf("other_%d", r.status)
}

type resv struct {
	ReservationID string   `json:"reservation_id"`
	UserID        string   `json:"user_id"`
	Seats         []string `json:"seats"`
}

func reserve(showID int64, user string, seats []string, key string, spoof bool) resp {
	user, key = uid(user), uid(key)
	body := map[string]any{"seats": seats, "idempotency_key": key}

	if spoof {
		body["user_id"] = "mallory" // must be ignored: identity comes from the token
	}
	return call("POST", fmt.Sprintf("/shows/%d/reserve", showID), token(user, "user"), body, nil)
}

// observe classifies a reserve response, records it, and updates what the server should now hold.
func observe(r resp, ph *tally) (string, resv) {
	c := classify(r)
	ph.add(c)
	overall.add(c)
	if r.err == nil {
		latMu.Lock()
		lats = append(lats, r.dur)
		latMu.Unlock()
	}
	var rv resv
	if c == "201_confirmed" || c == "201_replay" {
		json.Unmarshal(r.body, &rv)
		if (c == "201_confirmed" || c == "201_replay") && rv.ReservationID == "" {
			ph.add("bad_body")
			overall.add("bad_body")
			fmt.Printf("  ! 201 without a reservation_id: %.150s\n", r.body)
		}
	}
	if c == "201_confirmed" {
		newRes.Add(1)
		newSeats.Add(int64(len(rv.Seats)))
		netSeats.Add(int64(len(rv.Seats)))
	}
	if c == "5xx" || c == "network_error" {
		fmt.Printf("  ! %s: status=%d err=%v body=%.120s\n", c, r.status, r.err, r.body)
	}
	return c, rv
}

func fireAll(n int, fn func(i int)) {
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			fn(i)
		}(i)
	}
	close(start)
	wg.Wait()
}

// ---------- state + metrics ----------
type showState struct {
	TotalSeats int `json:"total_seats"`
	Available  int `json:"available"`
	Held       int `json:"held"`
	Confirmed  int `json:"confirmed"`
}

func getShow(id int64) (showState, error) {
	r := call("GET", fmt.Sprintf("/shows/%d?include_seats=false", id), "", nil, nil)
	if r.err != nil || r.status != 200 {
		return showState{}, fmt.Errorf("status %d err %v", r.status, r.err)
	}
	var s showState
	return s, json.Unmarshal(r.body, &s)
}

func scrape() string {
	r := call("GET", "/metrics", "", nil, nil)
	if r.err != nil || r.status != 200 {
		return ""
	}
	return string(r.body)
}

func metric(text, name, label string) float64 {
	var sum float64
	for _, ln := range strings.Split(text, "\n") {
		if ln == "" || ln[0] == '#' {
			continue
		}
		if !strings.HasPrefix(ln, name+"{") && !strings.HasPrefix(ln, name+" ") {
			continue
		}
		if label != "" && !strings.Contains(ln, label) {
			continue
		}
		f := strings.Fields(ln)
		if v, err := strconv.ParseFloat(f[len(f)-1], 64); err == nil {
			sum += v
		}
	}
	return sum
}

// two calls with the same key: both must have the same outcome class; if 201, same id, exactly one non-replay.
func checkPair(ca, cb string, a, b resv) {
	is201 := func(c string) bool { return strings.HasPrefix(c, "201") }
	if is201(ca) != is201(cb) {
		dupViol.Add(1)
		return
	}
	if is201(ca) && (a.ReservationID != b.ReservationID || (ca == "201_confirmed") == (cb == "201_confirmed")) {
		dupViol.Add(1)
	}
}

func main() {
	urlFlag := flag.String("url", os.Getenv("BASE_URL"), "base URL of the service")
	nSeats := flag.Int("seats", 2000, "seats in the test show (min 1300)")
	hotUsers := flag.Int("hot-users", 500, "users in the single-seat storm")
	total := flag.Int("n", 20000, "requests in the mixed stampede")
	conc := flag.Int("conc", 200, "max in-flight requests during the stampede")
	hot := flag.Int("hot", 20, "hot seats in the stampede (3 to 190)")
	flag.Parse()
	if *urlFlag == "" && flag.NArg() > 0 {
		*urlFlag = flag.Arg(0)
	}
	if *urlFlag == "" || os.Getenv("JWT_SECRET") == "" || *nSeats < 1300 || *hot < 3 || *hot > 190 {
		fmt.Fprintln(os.Stderr, "usage: JWT_SECRET=<server secret> burst -url <BASE_URL> [-n 20000 -conc 200 -hot 20 -seats 2000 -hot-users 500]")
		os.Exit(2)
	}
	base = strings.TrimRight(*urlFlag, "/")
	secret = []byte(os.Getenv("JWT_SECRET"))
	client = &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
		MaxIdleConns: 1000, MaxIdleConnsPerHost: 1000, IdleConnTimeout: 90 * time.Second}}

	// 0. wait for readiness (this is the cold-start measurement)
	t0 := time.Now()
	for {
		r := call("GET", "/readyz", "", nil, nil)
		if r.err == nil && r.status == 200 {
			break
		}
		if time.Since(t0) > 2*time.Minute {
			fmt.Println("service never became ready")
			os.Exit(1)
		}
		time.Sleep(2 * time.Second)
	}
	fmt.Printf("target %s ready after %s\n", base, time.Since(t0).Round(time.Millisecond))

	// 1. create a fresh show
	seats := make([]string, *nSeats)
	for i := range seats {
		seats[i] = fmt.Sprintf("S%d", i+1)
	}
	r := call("POST", "/shows", token("burst-admin", "admin"),
		map[string]any{"name": "burst-" + time.Now().Format("150405"), "seats": seats, "price_paise": 25000}, nil)
	var created struct {
		ID json.Number `json:"id"`
	}
	json.Unmarshal(r.body, &created)
	showID, err := created.ID.Int64()
	if r.status != 201 || err != nil {
		fmt.Printf("create show failed: status=%d err=%v body=%.200s\n", r.status, r.err, r.body)
		os.Exit(1)
	}
	fmt.Printf("created show %d with %d seats\n", showID, *nSeats)

	un := call("POST", "/shows", token("someone", "user"), map[string]any{"name": "x", "seats": []string{"A1"}, "price_paise": 1}, nil)
	expect("user token cannot create shows (403)", un.status == 403, "got %d", un.status)
	nt := call("POST", fmt.Sprintf("/shows/%d/reserve", showID), "", map[string]any{"seats": []string{"S1"}, "idempotency_key": "x"}, nil)
	expect("reserve without token is rejected (401)", nt.status == 401, "got %d", nt.status)

	before := scrape()

	// invariant poller: runs for the whole burst
	stopPoll := make(chan struct{})
	var pollWG sync.WaitGroup
	var polls, pollViol, pollErr atomic.Int64
	pollWG.Add(1)
	go func() {
		defer pollWG.Done()
		for {
			select {
			case <-stopPoll:
				return
			default:
			}
			if s, err := getShow(showID); err != nil {
				pollErr.Add(1)
			} else {
				polls.Add(1)
				if s.Available+s.Held+s.Confirmed != s.TotalSeats {
					pollViol.Add(1)
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	// 2. hot-seat storm: many users, one seat
	ph1 := newTally()
	phases = append(phases, phase{"hot-seat storm (S1)", ph1})
	fireAll(*hotUsers, func(i int) {
		observe(reserve(showID, fmt.Sprintf("hot-u-%d", i), []string{"S1"}, fmt.Sprintf("hot-k-%d", i), false), ph1)
	})
	expect("hot seat: exactly one winner", ph1.get("201_confirmed") == 1 && ph1.get("409_seat_taken") == *hotUsers-1 && ph1.total() == *hotUsers,
		"confirmed=%d taken=%d of %d", ph1.get("201_confirmed"), ph1.get("409_seat_taken"), *hotUsers)

	// 3. mixed stampede: hot seats + cold seats, retries, double-clicks
	ph2 := newTally()
	phases = append(phases, phase{fmt.Sprintf("mixed stampede (%d requests, %d hot seats)", *total, *hot), ph2})
	sem := make(chan struct{}, *conc)
	var wg sync.WaitGroup
	for i := 0; i < *total; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			cnt := 1 + rand.IntN(3)
			var picked []string
			if rand.IntN(10) < 8 {
				for _, p := range rand.Perm(*hot)[:cnt] {
					picked = append(picked, fmt.Sprintf("S%d", p+2)) // S2..S(hot+1)
				}
			} else {
				set := map[int]bool{}
				for len(set) < cnt {
					set[200+rand.IntN(800)] = true // cold seats S200..S999
				}
				for k := range set {
					picked = append(picked, fmt.Sprintf("S%d", k))
				}
			}
			user := fmt.Sprintf("u-%d", rand.IntN(2000))
			key := fmt.Sprintf("st-%d", i)

			switch {
			case i%10 == 0: // double-click: same key twice, in parallel
				var ra, rb resp
				var w sync.WaitGroup
				w.Add(2)
				go func() { defer w.Done(); ra = reserve(showID, user, picked, key, false) }()
				go func() { defer w.Done(); rb = reserve(showID, user, picked, key, false) }()
				w.Wait()
				ca, a := observe(ra, ph2)
				cb, b := observe(rb, ph2)
				checkPair(ca, cb, a, b)
			case i%7 == 0: // sequential retry: must return the original
				c1, a := observe(reserve(showID, user, picked, key, false), ph2)
				c2, b := observe(reserve(showID, user, picked, key, false), ph2)
				if c1 == "201_confirmed" && (c2 != "201_replay" || a.ReservationID != b.ReservationID) {
					seqViol.Add(1)
				}
			default:
				observe(reserve(showID, user, picked, key, false), ph2)
			}
		}(i)
	}
	wg.Wait()
	expect("stampede: no 5xx and no network errors", ph2.get("5xx") == 0 && ph2.get("network_error") == 0,
		"5xx=%d network=%d", ph2.get("5xx"), ph2.get("network_error"))
	expect("idempotency: parallel same-key pairs agree", dupViol.Load() == 0, "violations=%d", dupViol.Load())
	expect("idempotency: sequential retries return the original", seqViol.Load() == 0, "violations=%d", seqViol.Load())

	// 4. per-user limit: one user, 10 parallel requests, limit 4
	ph3 := newTally()
	phases = append(phases, phase{"per-user limit (1 user x 10 parallel)", ph3})
	fireAll(10, func(i int) {
		observe(reserve(showID, "limit-user", []string{fmt.Sprintf("S%d", 1000+i)}, fmt.Sprintf("lim-k-%d", i), false), ph3)
	})
	expect("per-user limit: at most 4 confirmed", ph3.get("201_confirmed") == 4 && ph3.get("409_over_limit") == 6 && ph3.total() == 10,
		"confirmed=%d over_limit=%d", ph3.get("201_confirmed"), ph3.get("409_over_limit"))

	// 5. same key in parallel, then same key with different seats
	ph4 := newTally()
	phases = append(phases, phase{"same key x 20 parallel", ph4})
	var idMu sync.Mutex
	ids := map[string]bool{}
	fireAll(20, func(i int) {
		c, rv := observe(reserve(showID, "dup-user", []string{"S1100", "S1101"}, "dup-key", false), ph4)
		if strings.HasPrefix(c, "201") {
			idMu.Lock()
			ids[rv.ReservationID] = true
			idMu.Unlock()
		}
	})
	expect("same key x20: one reservation, 19 replays", ph4.get("201_confirmed") == 1 && ph4.get("201_replay") == 19 && len(ids) == 1,
		"confirmed=%d replays=%d distinct_ids=%d", ph4.get("201_confirmed"), ph4.get("201_replay"), len(ids))
	ph5 := newTally()
	phases = append(phases, phase{"same key, different seats", ph5})
	c, _ := observe(reserve(showID, "dup-user", []string{"S1100", "S1102"}, "dup-key", false), ph5)
	expect("same key + different seats is rejected (409)", c == "409_key_conflict", "got %s", c)

	// 6. identity: spoofed body, non-owner cancel, cancel then rebook
	ph6 := newTally()
	phases = append(phases, phase{"identity and cancel", ph6})
	c, rv := observe(reserve(showID, "alice-b", []string{"S1200"}, "id-k1", true), ph6)
	expect("spoofed user_id in body is ignored", c == "201_confirmed" && rv.UserID == uid("alice-b"),
		"class=%s user_id=%q", c, rv.UserID)
	expect("reserve returned a reservation id", rv.ReservationID != "", "id=%q", rv.ReservationID)

	cancelPath := fmt.Sprintf("/reservations/%s/cancel", rv.ReservationID)
	rc := call("POST", cancelPath, token(uid("mallory-b"), "user"), nil, nil)
	expect("non-owner cancel is refused (404)", rc.status == 404, "got %d", rc.status)
	rc = call("POST", cancelPath, token(uid("alice-b"), "user"), nil, nil)
	expect("owner cancel succeeds (200)", rc.status == 200, "got %d", rc.status)
	if rc.status == 200 {
		netSeats.Add(-1)
		cancelledRes.Add(1)
	}
	rc = call("POST", cancelPath, token(uid("alice-b"), "user"), nil, nil)
	expect("repeat cancel is idempotent (200)", rc.status == 200, "got %d", rc.status)
	c, _ = observe(reserve(showID, "bob-b", []string{"S1200"}, "id-k2", false), ph6)
	expect("cancelled seat can be rebooked", c == "201_confirmed", "got %s", c)

	// 7. stop polling, let the 1s metrics cache expire, reconcile
	close(stopPoll)
	pollWG.Wait()
	expect("invariant held during the whole burst", pollViol.Load() == 0, "%d polls, %d violations, %d poll errors", polls.Load(), pollViol.Load(), pollErr.Load())
	time.Sleep(2 * time.Second)

	s, err := getShow(showID)
	expect("final: available + held + confirmed == total", err == nil && s.Available+s.Held+s.Confirmed == s.TotalSeats,
		"%+v err=%v", s, err)
	expect("final: confirmed seats == seats the API confirmed minus cancelled", int64(s.Confirmed) == netSeats.Load(),
		"server=%d client=%d", s.Confirmed, netSeats.Load())
	expect("final: nothing stuck in held", s.Held == 0, "held=%d", s.Held)

	after := scrape()
	if before == "" || after == "" {
		expect("metrics scrape", false, "could not read /metrics")
	} else {
		d := func(name, label string) int64 { return int64(metric(after, name, label) - metric(before, name, label)) }
		expect("metrics: reservations_confirmed_total == new reservations", d("reservations_confirmed_total", "") == newRes.Load(),
			"metric=%d client=%d", d("reservations_confirmed_total", ""), newRes.Load())
		expect("metrics: seats_confirmed_total == seats confirmed", d("seats_confirmed_total", "") == newSeats.Load(),
			"metric=%d client=%d", d("seats_confirmed_total", ""), newSeats.Load())
		expect("metrics: reservations_cancelled_total == cancels", d("reservations_cancelled_total", "") == cancelledRes.Load(),
			"metric=%d client=%d", d("reservations_cancelled_total", ""), cancelledRes.Load())
		expect("metrics: seats{confirmed} moved by the net confirmed seats", d("seats", `status="confirmed"`) == netSeats.Load(),
			"metric=%d client=%d", d("seats", `status="confirmed"`), netSeats.Load())
		expect("metrics: no 5xx recorded by the server", d("http_requests_total", `status="5`) == 0,
			"server 5xx delta=%d", d("http_requests_total", `status="5`))
		fmt.Printf("\ndeclined by reason (server metrics delta): seat_taken=%d per_user_limit=%d idempotent_replay=%d idempotency_conflict=%d\n",
			d("reservations_declined_total", `reason="seat_taken"`), d("reservations_declined_total", `reason="per_user_limit"`),
			d("reservations_declined_total", `reason="idempotent_replay"`), d("reservations_declined_total", `reason="idempotency_conflict"`))
		fmt.Printf("store retries (deadlock/timeout/missing_original): %d, exhausted: %d\n",
			d("store_retries_total", ""), d("store_retries_total", `reason="exhausted"`))
	}
	expect("client saw zero 5xx and zero network errors", overall.get("5xx") == 0 && overall.get("network_error") == 0,
		"5xx=%d network=%d", overall.get("5xx"), overall.get("network_error"))

	expect("every 201 carries a reservation_id", overall.get("bad_body") == 0, "bad bodies=%d", overall.get("bad_body"))

	// ---------- report ----------
	fmt.Println("\n=== outcome distribution ===")
	for _, p := range phases {
		fmt.Printf("\n%s\n", p.name)
		p.t.mu.Lock()
		keys := make([]string, 0, len(p.t.m))
		for k := range p.t.m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("  %-18s %d\n", k, p.t.m[k])
		}
		p.t.mu.Unlock()
	}
	fmt.Println("\noverall (all reserve calls)")
	overall.mu.Lock()
	keys := make([]string, 0, len(overall.m))
	for k := range overall.m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-18s %d\n", k, overall.m[k])
	}
	overall.mu.Unlock()

	latMu.Lock()
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	if n := len(lats); n > 0 {
		q := func(p float64) time.Duration { return lats[int(float64(n-1)*p)].Round(time.Millisecond) }
		fmt.Printf("\nlatency of reserve calls: p50=%s p95=%s p99=%s max=%s\n", q(.50), q(.95), q(.99), lats[n-1].Round(time.Millisecond))
	}
	latMu.Unlock()
	fmt.Printf("final state of show %d: %+v\n", showID, s)

	fmt.Println("\n=== checks ===")
	failed := 0
	for _, c := range checks {
		tag := "PASS"
		if !c.ok {
			tag = "FAIL"
			failed++
		}
		fmt.Printf("[%s] %-62s %s\n", tag, c.name, c.detail)
	}
	if failed > 0 {
		fmt.Printf("\n%d check(s) failed\n", failed)
		os.Exit(1)
	}
	fmt.Println("\nall checks passed")
}
