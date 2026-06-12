package cmd

// board_test.go covers the kanban board's Bubble Tea model and HTTP helpers.
// Model tests are pure (no server needed); HTTP tests use recordingServer.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// makeBoard builds a model with the given cards in column 0 and optional
// cards in column 1, placing the cursor at col/row.
func makeBoard(col0, col1 []boardTicket, col, row int) boardModel {
	cols := make([][]boardTicket, len(defaultBoardStatuses))
	for i := range cols {
		cols[i] = []boardTicket{}
	}
	if col0 != nil {
		cols[0] = col0
	}
	if col1 != nil {
		cols[1] = col1
	}
	return boardModel{
		statuses:   defaultBoardStatuses,
		priorities: defaultBoardPriorities,
		cols:       cols,
		col:        col,
		row:        row,
	}
}

// fakeTickets returns n trivial boardTickets.
func fakeTickets(n int) []boardTicket {
	out := make([]boardTicket, n)
	for i := range out {
		out[i] = boardTicket{
			ID:       testUUID,
			Title:    "ticket",
			Priority: "medium",
			Status:   "open",
		}
	}
	return out
}

// updateKey sends a key string to m and returns the new boardModel.
func updateKey(m boardModel, key string) boardModel {
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
	return next.(boardModel)
}

func updateSpecialKey(m boardModel, kt tea.KeyType) boardModel {
	next, _ := m.Update(tea.KeyMsg{Type: kt})
	return next.(boardModel)
}

// boardDataMsgDefaults returns a boardDataMsg with default statuses/priorities
// and empty columns — suitable for tests that just want loading to complete.
func boardDataMsgDefaults() boardDataMsg {
	cols := make([][]boardTicket, len(defaultBoardStatuses))
	for i := range cols {
		cols[i] = []boardTicket{}
	}
	return boardDataMsg{
		statuses:   defaultBoardStatuses,
		priorities: defaultBoardPriorities,
		cols:       cols,
	}
}

// boardFetchServer starts a routeServer that serves empty field-defs (so the
// board falls back to defaults) and ticketBody / ticketStatus for /tickets/tickets.
func boardFetchServer(t *testing.T, ticketStatus int, ticketBody string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/tickets/field-defs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]")) //nolint:errcheck
	})
	mux.HandleFunc("/tickets/tickets", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(ticketStatus)
		if ticketBody != "" {
			w.Write([]byte(ticketBody)) //nolint:errcheck
		}
	})
	return routeServer(t, mux)
}

// ── boardClamp ────────────────────────────────────────────────────────────────

func TestBoardClamp_Empty(t *testing.T) {
	if got := boardClamp(5, 0); got != 5 {
		t.Errorf("clamp(5,0) = %d, want 5 (no clamping when empty)", got)
	}
}

func TestBoardClamp_WithinBounds(t *testing.T) {
	if got := boardClamp(2, 5); got != 2 {
		t.Errorf("clamp(2,5) = %d, want 2", got)
	}
}

func TestBoardClamp_AtBoundary(t *testing.T) {
	if got := boardClamp(4, 5); got != 4 {
		t.Errorf("clamp(4,5) = %d, want 4", got)
	}
}

func TestBoardClamp_OutOfBounds(t *testing.T) {
	if got := boardClamp(5, 5); got != 4 {
		t.Errorf("clamp(5,5) = %d, want 4", got)
	}
}

// ── focusedCard ───────────────────────────────────────────────────────────────

func TestFocusedCard_EmptyColumn(t *testing.T) {
	m := makeBoard(nil, nil, 0, 0)
	if _, ok := m.focusedCard(); ok {
		t.Error("expected no card from empty column")
	}
}

func TestFocusedCard_ReturnsCard(t *testing.T) {
	tickets := fakeTickets(3)
	tickets[1].Title = "target"
	m := makeBoard(tickets, nil, 0, 1)
	card, ok := m.focusedCard()
	if !ok {
		t.Fatal("expected a card, got none")
	}
	if card.Title != "target" {
		t.Errorf("card.Title = %q, want %q", card.Title, "target")
	}
}

func TestFocusedCard_RowClampedAfterColumnSwitch(t *testing.T) {
	col0 := fakeTickets(5)
	col1 := fakeTickets(1)
	col1[0].Title = "solo"
	m := makeBoard(col0, col1, 0, 4) // cursor at last card of col0

	// switch right — row should clamp to 0 because col1 has only 1 card
	m2 := updateSpecialKey(m, tea.KeyRight)
	card, ok := m2.focusedCard()
	if !ok {
		t.Fatal("expected a card after column switch")
	}
	if card.Title != "solo" {
		t.Errorf("card = %q, want %q", card.Title, "solo")
	}
}

// ── navigation ────────────────────────────────────────────────────────────────

func TestNavigation_RightIncreasesCol(t *testing.T) {
	m := makeBoard(fakeTickets(1), fakeTickets(1), 0, 0)
	m2 := updateSpecialKey(m, tea.KeyRight)
	if m2.col != 1 {
		t.Errorf("col = %d, want 1", m2.col)
	}
}

func TestNavigation_RightAtEdge_NoOp(t *testing.T) {
	m := makeBoard(nil, nil, 3, 0)
	m2 := updateKey(m, "l")
	if m2.col != 3 {
		t.Errorf("col = %d, want 3 (already at edge)", m2.col)
	}
}

func TestNavigation_LeftDecreasesCol(t *testing.T) {
	m := makeBoard(fakeTickets(1), fakeTickets(1), 1, 0)
	m2 := updateSpecialKey(m, tea.KeyLeft)
	if m2.col != 0 {
		t.Errorf("col = %d, want 0", m2.col)
	}
}

func TestNavigation_LeftAtEdge_NoOp(t *testing.T) {
	m := makeBoard(nil, nil, 0, 0)
	m2 := updateKey(m, "h")
	if m2.col != 0 {
		t.Errorf("col = %d, want 0 (already at edge)", m2.col)
	}
}

func TestNavigation_DownIncreasesRow(t *testing.T) {
	m := makeBoard(fakeTickets(3), nil, 0, 0)
	m2 := updateSpecialKey(m, tea.KeyDown)
	if m2.row != 1 {
		t.Errorf("row = %d, want 1", m2.row)
	}
}

func TestNavigation_DownAtBottom_NoOp(t *testing.T) {
	m := makeBoard(fakeTickets(2), nil, 0, 1)
	m2 := updateKey(m, "j")
	if m2.row != 1 {
		t.Errorf("row = %d, want 1 (already at bottom)", m2.row)
	}
}

func TestNavigation_UpDecreasesRow(t *testing.T) {
	m := makeBoard(fakeTickets(3), nil, 0, 2)
	m2 := updateSpecialKey(m, tea.KeyUp)
	if m2.row != 1 {
		t.Errorf("row = %d, want 1", m2.row)
	}
}

func TestNavigation_UpAtTop_NoOp(t *testing.T) {
	m := makeBoard(fakeTickets(3), nil, 0, 0)
	m2 := updateKey(m, "k")
	if m2.row != 0 {
		t.Errorf("row = %d, want 0 (already at top)", m2.row)
	}
}

// ── loading state ─────────────────────────────────────────────────────────────

func TestUpdate_KeysIgnoredWhileLoading(t *testing.T) {
	cols := make([][]boardTicket, len(defaultBoardStatuses))
	for i := range cols {
		cols[i] = []boardTicket{}
	}
	m := boardModel{
		loading:    true,
		statuses:   defaultBoardStatuses,
		priorities: defaultBoardPriorities,
		cols:       cols,
		col:        0,
		row:        0,
	}
	m.cols[0] = fakeTickets(3)

	for _, key := range []string{"j", "k", "h", "l", "r"} {
		m2 := updateKey(m, key)
		if m2.col != 0 || m2.row != 0 {
			t.Errorf("key %q changed state while loading: col=%d row=%d", key, m2.col, m2.row)
		}
	}
}

// ── message handling ──────────────────────────────────────────────────────────

func TestUpdate_BoardLoadedMsg_PopulatesColumns(t *testing.T) {
	m := boardModel{loading: true}
	msg := boardDataMsgDefaults()
	msg.cols[0] = fakeTickets(2)
	msg.cols[2] = fakeTickets(1)

	next, _ := m.Update(msg)
	m2 := next.(boardModel)

	if m2.loading {
		t.Error("loading should be false after boardDataMsg")
	}
	if m2.err != nil {
		t.Errorf("err should be nil, got %v", m2.err)
	}
	if len(m2.cols[0]) != 2 {
		t.Errorf("col[0] len = %d, want 2", len(m2.cols[0]))
	}
	if len(m2.cols[2]) != 1 {
		t.Errorf("col[2] len = %d, want 1", len(m2.cols[2]))
	}
}

func TestUpdate_BoardLoadedMsg_ClearRefreshStatus(t *testing.T) {
	m := boardModel{loading: true, status: "Refreshing…"}
	next, _ := m.Update(boardDataMsgDefaults())
	m2 := next.(boardModel)
	if m2.status != "" {
		t.Errorf("status = %q, want empty after refresh", m2.status)
	}
}

func TestUpdate_BoardLoadedMsg_PreservesOtherStatus(t *testing.T) {
	m := boardModel{loading: true, status: "Moved."}
	next, _ := m.Update(boardDataMsgDefaults())
	m2 := next.(boardModel)
	if m2.status != "Moved." {
		t.Errorf("status = %q, want %q", m2.status, "Moved.")
	}
}

func TestUpdate_BoardErrMsg_SetsError(t *testing.T) {
	m := boardModel{loading: true}
	next, _ := m.Update(boardErrMsg{err: errFixture("api down")})
	m2 := next.(boardModel)

	if m2.loading {
		t.Error("loading should be false after errMsg")
	}
	if m2.err == nil || !strings.Contains(m2.err.Error(), "api down") {
		t.Errorf("err = %v, want 'api down'", m2.err)
	}
}

func TestUpdate_BoardMovedMsg_SetsStatusAndRefetches(t *testing.T) {
	m := boardModel{}
	next, cmd := m.Update(boardMovedMsg{})
	m2 := next.(boardModel)
	if m2.status != "Moved." {
		t.Errorf("status = %q, want 'Moved.'", m2.status)
	}
	if cmd == nil {
		t.Fatal("boardMovedMsg should return a fetch command")
	}
}

func TestUpdate_WindowSizeMsg(t *testing.T) {
	m := boardModel{}
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m2 := next.(boardModel)
	if m2.width != 120 || m2.height != 40 {
		t.Errorf("size = %dx%d, want 120x40", m2.width, m2.height)
	}
}

// ── quit ──────────────────────────────────────────────────────────────────────

func TestUpdate_QuitKeys(t *testing.T) {
	for _, key := range []string{"q"} {
		m := boardModel{}
		_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		if cmd == nil {
			t.Errorf("key %q: expected home command, got nil", key)
		}
		if _, ok := cmd().(goHomeMsg); !ok {
			t.Errorf("key %q: cmd() did not return goHomeMsg", key)
		}
	}
}

func TestUpdate_EscKey_GoesHome(t *testing.T) {
	m := boardModel{}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc: expected home command")
	}
	if _, ok := cmd().(goHomeMsg); !ok {
		t.Error("esc: cmd() did not return goHomeMsg")
	}
}

// ── H / L card movement ───────────────────────────────────────────────────────

func TestShiftRight_EmptyColumn_NoOp(t *testing.T) {
	m := makeBoard(nil, nil, 0, 0)
	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("L")})
	bm := m2.(boardModel)
	if bm.col != 0 {
		t.Errorf("col = %d, want 0", bm.col)
	}
	if cmd != nil {
		t.Error("empty column shift should produce no command")
	}
}

func TestShiftLeft_AtLeftEdge_NoOp(t *testing.T) {
	m := makeBoard(fakeTickets(2), nil, 0, 0)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("H")})
	if cmd != nil {
		t.Error("shift left at col 0 should produce no command")
	}
}

func TestShiftRight_AtRightEdge_NoOp(t *testing.T) {
	m := makeBoard(nil, nil, 3, 0)
	m.cols[3] = fakeTickets(1)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("L")})
	if cmd != nil {
		t.Error("shift right at col 3 should produce no command")
	}
}

func TestShiftRight_ProducesCommand(t *testing.T) {
	m := makeBoard(fakeTickets(1), nil, 0, 0)
	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("L")})
	bm := m2.(boardModel)
	if cmd == nil {
		t.Fatal("L on non-empty col should produce a command")
	}
	if bm.status != "Moving…" {
		t.Errorf("status = %q, want 'Moving…'", bm.status)
	}
}

func TestShiftLeft_ProducesCommand(t *testing.T) {
	m := makeBoard(nil, fakeTickets(1), 1, 0)
	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("H")})
	bm := m2.(boardModel)
	if cmd == nil {
		t.Fatal("H on non-empty col should produce a command")
	}
	if bm.status != "Moving…" {
		t.Errorf("status = %q, want 'Moving…'", bm.status)
	}
}

// ── refresh ───────────────────────────────────────────────────────────────────

func TestUpdate_RKey_SetsLoadingAndRefetches(t *testing.T) {
	m := boardModel{}
	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	bm := m2.(boardModel)
	if !bm.loading {
		t.Error("r: loading should be true")
	}
	if bm.status != "Refreshing…" {
		t.Errorf("r: status = %q, want 'Refreshing…'", bm.status)
	}
	if cmd == nil {
		t.Error("r: should return a command")
	}
}

// ── view ──────────────────────────────────────────────────────────────────────

func TestView_LoadingState(t *testing.T) {
	m := boardModel{loading: true}
	if !strings.Contains(m.View(), "Loading") {
		t.Error("loading view should mention Loading")
	}
}

func TestView_ErrorState(t *testing.T) {
	m := boardModel{err: errFixture("connection refused")}
	v := m.View()
	if !strings.Contains(v, "connection refused") {
		t.Error("error view should show the error message")
	}
	if !strings.Contains(v, "retry") {
		t.Error("error view should mention retry")
	}
}

func TestView_ErrorState_401_ShowsAuthHint(t *testing.T) {
	m := boardModel{err: errFixture("HTTP 401: unauthorized")}
	v := m.View()
	if !strings.Contains(v, "auth login") {
		t.Error("401 error view should hint at armory auth login")
	}
}

func TestView_NormalState_ContainsAllColumns(t *testing.T) {
	m := boardModel{
		statuses:   defaultBoardStatuses,
		priorities: defaultBoardPriorities,
		cols:       make([][]boardTicket, len(defaultBoardStatuses)),
	}
	v := m.View()
	for _, label := range []string{"Open", "In Progress", "Resolved", "Closed"} {
		if !strings.Contains(v, label) {
			t.Errorf("view missing column label %q", label)
		}
	}
}

func TestView_ShowsHelpLine(t *testing.T) {
	m := boardModel{
		statuses:   defaultBoardStatuses,
		priorities: defaultBoardPriorities,
		cols:       make([][]boardTicket, len(defaultBoardStatuses)),
	}
	v := m.View()
	if !strings.Contains(v, "home") {
		t.Error("view should contain help text mentioning 'home'")
	}
}

func TestView_StatusPrefix(t *testing.T) {
	m := boardModel{
		statuses:   defaultBoardStatuses,
		priorities: defaultBoardPriorities,
		cols:       make([][]boardTicket, len(defaultBoardStatuses)),
		status:     "Moved.",
	}
	v := m.View()
	if !strings.Contains(v, "Moved.") {
		t.Error("view should show status message when set")
	}
}

// ── HTTP: fetchBoardData ──────────────────────────────────────────────────────

func TestFetchBoardData_Success(t *testing.T) {
	payload := []boardTicket{
		{ID: "t1", Title: "Bug", Priority: "high", Status: "open"},
		{ID: "t2", Title: "Feat", Priority: "low", Status: "in_progress"},
		{ID: "t3", Title: "Docs", Priority: "medium", Status: "resolved"},
		{ID: "t4", Title: "CI", Priority: "critical", Status: "closed"},
	}
	body, _ := json.Marshal(payload)
	srv := boardFetchServer(t, http.StatusOK, string(body))
	setupCLI(t, srv)

	msg := fetchBoardData()

	loaded, ok := msg.(boardDataMsg)
	if !ok {
		t.Fatalf("expected boardDataMsg, got %T", msg)
	}
	if len(loaded.cols[0]) != 1 || loaded.cols[0][0].ID != "t1" {
		t.Errorf("col[0] (open) = %v", loaded.cols[0])
	}
	if len(loaded.cols[1]) != 1 || loaded.cols[1][0].ID != "t2" {
		t.Errorf("col[1] (in_progress) = %v", loaded.cols[1])
	}
	if len(loaded.cols[2]) != 1 || loaded.cols[2][0].ID != "t3" {
		t.Errorf("col[2] (resolved) = %v", loaded.cols[2])
	}
	if len(loaded.cols[3]) != 1 || loaded.cols[3][0].ID != "t4" {
		t.Errorf("col[3] (closed) = %v", loaded.cols[3])
	}
}

func TestFetchBoardData_EmptyList(t *testing.T) {
	srv := boardFetchServer(t, http.StatusOK, `[]`)
	setupCLI(t, srv)

	msg := fetchBoardData()
	loaded, ok := msg.(boardDataMsg)
	if !ok {
		t.Fatalf("expected boardDataMsg, got %T", msg)
	}
	for i, col := range loaded.cols {
		if len(col) != 0 {
			t.Errorf("col[%d] should be empty, has %d tickets", i, len(col))
		}
	}
}

func TestFetchBoardData_HTTPError(t *testing.T) {
	srv := boardFetchServer(t, http.StatusInternalServerError, `internal error`)
	setupCLI(t, srv)

	msg := fetchBoardData()
	if _, ok := msg.(boardErrMsg); !ok {
		t.Fatalf("expected boardErrMsg on HTTP error, got %T", msg)
	}
}

func TestFetchBoardData_InvalidJSON(t *testing.T) {
	srv := boardFetchServer(t, http.StatusOK, `not json`)
	setupCLI(t, srv)

	msg := fetchBoardData()
	if _, ok := msg.(boardErrMsg); !ok {
		t.Fatalf("expected boardErrMsg on bad JSON, got %T", msg)
	}
}

func TestFetchBoardData_UnknownStatusIgnored(t *testing.T) {
	payload := `[{"ticket_id":"t1","title":"T","priority":"low","status":"pending"}]`
	srv := boardFetchServer(t, http.StatusOK, payload)
	setupCLI(t, srv)

	msg := fetchBoardData()
	loaded, ok := msg.(boardDataMsg)
	if !ok {
		t.Fatalf("expected boardDataMsg, got %T", msg)
	}
	total := 0
	for _, col := range loaded.cols {
		total += len(col)
	}
	if total != 0 {
		t.Errorf("unknown status should be dropped, got %d tickets in cols", total)
	}
}

// ── HTTP: sendMoveTicket ──────────────────────────────────────────────────────

func TestSendMoveTicket_PutsCorrectEndpointAndBody(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)

	ticket := boardTicket{ID: "abc-123", Title: "Fix crash", Priority: "high", Status: "open"}
	cmd := sendMoveTicket(ticket, "in_progress")
	msg := cmd()

	if _, ok := msg.(boardMovedMsg); !ok {
		t.Fatalf("expected boardMovedMsg, got %T", msg)
	}
	if rec.Method != "PUT" {
		t.Errorf("method = %q, want PUT", rec.Method)
	}
	if rec.Path != "/tickets/tickets/abc-123" {
		t.Errorf("path = %q, want /tickets/tickets/abc-123", rec.Path)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body, &body); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if body["title"] != "Fix crash" {
		t.Errorf("body title = %q, want 'Fix crash'", body["title"])
	}
	if body["status"] != "in_progress" {
		t.Errorf("body status = %q, want 'in_progress'", body["status"])
	}
}

func TestSendMoveTicket_HTTPError_ReturnsBoardErrMsg(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusForbidden, `forbidden`)
	setupCLI(t, srv)

	cmd := sendMoveTicket(boardTicket{ID: "x", Title: "T"}, "resolved")
	msg := cmd()

	if _, ok := msg.(boardErrMsg); !ok {
		t.Fatalf("expected boardErrMsg on HTTP error, got %T", msg)
	}
}

// ── init command ──────────────────────────────────────────────────────────────

func TestBoardCmd_RegisteredUnderTickets(t *testing.T) {
	if findSubcmd(t, ticketsCmd, "board") == nil {
		t.Error("board subcommand not registered under tickets")
	}
}

// ── test-only helper ──────────────────────────────────────────────────────────

type testError struct{ msg string }

func (e testError) Error() string { return e.msg }

func errFixture(msg string) error { return testError{msg} }
