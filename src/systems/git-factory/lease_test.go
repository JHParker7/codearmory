package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The maintenance lease. What these tests pin down is the property the sweep depends
// on — at most one holder at a time — plus the two ways a lease is meant to move: the
// holder gives it up, or the holder dies and it expires.

func TestAcquireLease_SecondReplicaIsRefusedWhileItIsHeld(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()

	got, err := acquireLease(ctx, "sweep", "pod-a", time.Minute)
	if err != nil {
		t.Fatalf("acquire (a): %v", err)
	}
	if !got {
		t.Fatal("the first replica to ask was refused a free lease")
	}

	// The one that matters. Both replicas tick on the same interval, so this is the
	// ordinary case rather than an edge: if B were also granted, both would walk the
	// whole store and the lease would be buying nothing.
	for _, pod := range []string{"pod-b", "pod-c"} {
		got, err := acquireLease(ctx, "sweep", pod, time.Minute)
		if err != nil {
			t.Fatalf("acquire (%s): %v", pod, err)
		}
		if got {
			t.Errorf("%s was granted a lease already held by pod-a", pod)
		}
	}
}

// Several replicas asking at once is the real arrival pattern, since they all tick on
// the same interval. Exactly one may win.
func TestAcquireLease_ConcurrentReplicasProduceOneWinner(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()

	const replicas = 8
	var mu sync.Mutex
	winners := 0
	var wg sync.WaitGroup
	for i := range replicas {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// SQLite serialises writers and returns "database is locked" rather than
			// waiting, which is a property of the test engine and not of the lease, so
			// a contended call is retried. The assertion below is unaffected: retries
			// can only ever produce MORE winners, never fewer, so the test cannot pass
			// by accident.
			for attempt := 0; attempt < 50; attempt++ {
				got, err := acquireLease(ctx, "sweep", uuid.New().String(), time.Minute)
				if err != nil {
					time.Sleep(2 * time.Millisecond)
					continue
				}
				if got {
					mu.Lock()
					winners++
					mu.Unlock()
				}
				return
			}
		}(i)
	}
	wg.Wait()

	if winners != 1 {
		t.Errorf("%d replicas won the lease concurrently; want exactly 1", winners)
	}
}

func TestAcquireLease_TakenOverOnceTheHolderStopsRenewing(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()

	// A TTL already in the past stands in for the case this exists to survive: a pod
	// killed mid-sweep, which never released and will never renew.
	if _, err := acquireLease(ctx, "sweep", "pod-a", -time.Second); err != nil {
		t.Fatalf("acquire (a): %v", err)
	}

	got, err := acquireLease(ctx, "sweep", "pod-b", time.Minute)
	if err != nil {
		t.Fatalf("acquire (b): %v", err)
	}
	if !got {
		t.Error("an expired lease was not taken over — maintenance would stop until a restart")
	}
}

func TestRenewLease_OnlyForTheCurrentHolder(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()

	if _, err := acquireLease(ctx, "sweep", "pod-a", time.Minute); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	ok, err := renewLease(ctx, "sweep", "pod-a", time.Minute)
	if err != nil {
		t.Fatalf("renew (holder): %v", err)
	}
	if !ok {
		t.Error("the holder could not renew its own lease")
	}

	ok, err = renewLease(ctx, "sweep", "pod-b", time.Minute)
	if err != nil {
		t.Fatalf("renew (non-holder): %v", err)
	}
	if ok {
		t.Error("a replica that never held the lease was allowed to renew it")
	}
}

// The stalled-holder case. A pod that loses its lease to an expiry must find out
// rather than renew its way back in underneath whoever took over.
func TestRenewLease_FailsAfterAnotherReplicaTookOver(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()

	if _, err := acquireLease(ctx, "sweep", "pod-a", -time.Second); err != nil {
		t.Fatalf("acquire (a): %v", err)
	}
	if _, err := acquireLease(ctx, "sweep", "pod-b", time.Minute); err != nil {
		t.Fatalf("acquire (b): %v", err)
	}

	ok, err := renewLease(ctx, "sweep", "pod-a", time.Minute)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if ok {
		t.Error("pod-a renewed a lease pod-b now holds — two replicas would sweep believing they were alone")
	}
}

func TestReleaseLease_FreesItForTheNextTick(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()

	if _, err := acquireLease(ctx, "sweep", "pod-a", time.Hour); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := releaseLease(ctx, "sweep", "pod-a"); err != nil {
		t.Fatalf("release: %v", err)
	}

	// Without the release this would have to wait out the full hour.
	got, err := acquireLease(ctx, "sweep", "pod-b", time.Minute)
	if err != nil {
		t.Fatalf("acquire (b): %v", err)
	}
	if !got {
		t.Error("a released lease was not available to another replica")
	}
}

// Releasing is scoped to the holder, so a pod finishing a sweep it had already lost
// cannot expire the new holder's grant on its way out.
func TestReleaseLease_DoesNotDisturbANewHolder(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()

	if _, err := acquireLease(ctx, "sweep", "pod-a", -time.Second); err != nil {
		t.Fatalf("acquire (a): %v", err)
	}
	if _, err := acquireLease(ctx, "sweep", "pod-b", time.Minute); err != nil {
		t.Fatalf("acquire (b): %v", err)
	}

	if err := releaseLease(ctx, "sweep", "pod-a"); err != nil {
		t.Fatalf("release (a): %v", err)
	}

	got, err := acquireLease(ctx, "sweep", "pod-c", time.Minute)
	if err != nil {
		t.Fatalf("acquire (c): %v", err)
	}
	if got {
		t.Error("pod-a's release freed pod-b's lease, letting a third replica in")
	}
}

// Leases are named so a second periodic job can be added without inheriting this
// one's schedule.
func TestLeases_AreIndependentByName(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()

	if _, err := acquireLease(ctx, "sweep", "pod-a", time.Minute); err != nil {
		t.Fatalf("acquire (sweep): %v", err)
	}
	got, err := acquireLease(ctx, "other-job", "pod-a", time.Minute)
	if err != nil {
		t.Fatalf("acquire (other-job): %v", err)
	}
	if !got {
		t.Error("taking one lease blocked an unrelated one")
	}
}

func TestMaintenanceLeaseTTL(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", 5 * time.Minute},
		{"30s", 30 * time.Second},
		{"nonsense", 5 * time.Minute},
		// A non-positive TTL would grant a lease that is expired on arrival, so every
		// replica would sweep every tick — the bug this whole file exists to fix.
		{"0", 5 * time.Minute},
		{"-1m", 5 * time.Minute},
		// The floor. The renewal ticker runs at a third of the TTL, and time.NewTicker
		// panics on a non-positive interval — anything under 3ns divides to zero, which
		// would take the process down from a goroutine on the first sweep rather than
		// at startup.
		{"1ns", 5 * time.Minute},
		{"999ms", 5 * time.Minute},
		{"1s", time.Second},
	}
	for _, c := range cases {
		t.Setenv("GIT_MAINTENANCE_LEASE_TTL", c.env)
		if got := maintenanceLeaseTTL(); got != c.want {
			t.Errorf("GIT_MAINTENANCE_LEASE_TTL=%q gave %v; want %v", c.env, got, c.want)
		}
	}
}

// Losing the lease stops the sweep at the next repo boundary rather than immediately:
// an interrupted `git gc` leaves a gc.pid behind that blocks the next one.
func TestRunMaintenanceWhile_StopsAtARepoBoundary(t *testing.T) {
	setupTestDB(t)
	ctx := context.Background()
	for _, name := range []string{"one", "two", "three"} {
		seedRepoVisible(t, uuid.New().String(), "user-1", "admin", name, visibilityPrivate)
	}

	calls := 0
	swept, _, _ := runMaintenanceWhile(ctx, func() bool {
		calls++
		return calls <= 1 // lease lost after the first repo
	})
	if swept != 1 {
		t.Errorf("swept %d repos after losing the lease; want 1 (the one already in flight)", swept)
	}
}
