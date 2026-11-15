//go:build integration

package main

// Regression tests for orphaned pending_print_snapshots rows.
//
// Symptom: snapshots from an old print (typically "Attention @ 0%") appeared in the
// live view of every later print on the same printer, and were never cleaned up.
//
// Causes covered here:
//   1. printStartTime was never cleared when a print ended, so an ATTENTION/PAUSE
//      seen before the next print was detected was filed under the previous,
//      already-flushed print — and could never be flushed.
//   2. The active-snapshot views selected "the newest batch on this printer" rather
//      than "this print's batch", so a print with no snapshots of its own showed
//      the newest orphaned batch.
//   3. Nothing ever removed orphaned rows or their image files.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestPendingSnapshots_PrePrintAttentionNotOrphaned reproduces the production bug on
// PrusaLink: print A finishes, then print B enters ATTENTION at 0% (e.g. the job was
// started with the printer's door open) before the monitor ever sees it PRINTING.
// That snapshot used to be stamped with print A's start time and orphaned forever.
func TestPendingSnapshots_PrePrintAttentionNotOrphaned(t *testing.T) {
	const spoolID = 70
	bridge, printer, _ := setupBridgeWithMocks(t, map[int]float64{spoolID: 500.0})
	t.Setenv("THE_MOMENT_GCODE_PATH", t.TempDir())
	if err := bridge.SetToolheadMapping("Core One L", 0, spoolID); err != nil {
		t.Fatalf("SetToolheadMapping: %v", err)
	}
	printer.SetGcodeUsage(map[int]float64{0: 10.0})
	printer.SetCameraSnapshot(minimalJPEG)

	// Print A runs to completion.
	printer.SetJobID(300)
	printer.SetState(StatePrinting)
	printer.SetProgress(50)
	poll(t, bridge, printer, "Core One L", 1)
	printer.SetState(StateFinished)
	poll(t, bridge, printer, "Core One L", 1)
	if n := countPendingSnapshots(t, bridge, "test-printer-id"); n != 0 {
		t.Fatalf("after print A: expected 0 pending snapshots, got %d", n)
	}

	// Print B hits ATTENTION at 0% before it is ever seen PRINTING.
	printer.SetJobID(301)
	printer.SetProgress(0)
	printer.SetState(StateAttention)
	poll(t, bridge, printer, "Core One L", 1)
	if n := countPendingSnapshots(t, bridge, "test-printer-id"); n != 0 {
		t.Errorf("pre-print ATTENTION: expected 0 pending snapshots (would be orphaned under print A), got %d", n)
	}

	// Print B then starts. Its first PRINTING poll follows ATTENTION, but no
	// interruption of print B was recorded, so no unpaired "Resumed" snapshot either.
	printer.SetState(StatePrinting)
	printer.SetProgress(40)
	poll(t, bridge, printer, "Core One L", 1)
	if n := countPendingSnapshots(t, bridge, "test-printer-id"); n != 0 {
		t.Errorf("first PRINTING poll after pre-print ATTENTION: expected 0 pending snapshots, got %d", n)
	}

	// Print B finishes; nothing may be left behind.
	printer.SetState(StateFinished)
	poll(t, bridge, printer, "Core One L", 1)
	if n := countPendingSnapshots(t, bridge, "test-printer-id"); n != 0 {
		t.Errorf("after print B: expected 0 pending snapshots, got %d", n)
	}
}

// TestPendingSnapshots_StartTimeClearedOnEnd checks the invariant directly for both
// the finished and cancelled PrusaLink paths.
func TestPendingSnapshots_StartTimeClearedOnEnd(t *testing.T) {
	for _, endState := range []string{StateFinished, StateStopped} {
		t.Run(endState, func(t *testing.T) {
			const spoolID = 71
			bridge, printer, _ := setupBridgeWithMocks(t, map[int]float64{spoolID: 500.0})
			t.Setenv("THE_MOMENT_GCODE_PATH", t.TempDir())
			if err := bridge.SetToolheadMapping("Core One L", 0, spoolID); err != nil {
				t.Fatalf("SetToolheadMapping: %v", err)
			}
			printer.SetGcodeUsage(map[int]float64{0: 10.0})
			printer.SetJobID(310)
			printer.SetState(StatePrinting)
			printer.SetProgress(50)
			poll(t, bridge, printer, "Core One L", 1)

			bridge.mutex.RLock()
			started := bridge.printStartTime["test-printer-id"]
			bridge.mutex.RUnlock()
			if started.IsZero() {
				t.Fatal("printStartTime not set while printing")
			}

			printer.SetState(endState)
			poll(t, bridge, printer, "Core One L", 1)

			bridge.mutex.RLock()
			after := bridge.printStartTime["test-printer-id"]
			bridge.mutex.RUnlock()
			if !after.IsZero() {
				t.Errorf("printStartTime still %v after %s", after, endState)
			}
		})
	}
}

// TestPendingSnapshots_SurviveRestartMidPrint guards the purge against a service
// restart during a print. The current print is identified by its in-memory start
// time, which ReconcileActiveSessions rebuilds from the saved session. If that
// rebuild lost the start time or the job file, the first poll after the restart
// would look like a new print and the purge would delete this print's own snapshots.
func TestPendingSnapshots_SurviveRestartMidPrint(t *testing.T) {
	const spoolID = 74
	// Set before the first bridge is built: the path is read at construction, and
	// both bridges must share it, as they would across a real restart.
	t.Setenv("THE_MOMENT_GCODE_PATH", t.TempDir())
	bridge, printer, spoolman := setupBridgeWithMocks(t, map[int]float64{spoolID: 500.0})
	if err := bridge.SetToolheadMapping("Core One L", 0, spoolID); err != nil {
		t.Fatalf("SetToolheadMapping: %v", err)
	}
	printer.SetGcodeUsage(map[int]float64{0: 10.0})
	printer.SetCameraSnapshot(minimalJPEG)

	// A print starts, is interrupted and resumes: two staged snapshots.
	printer.SetJobID(340)
	printer.SetState(StatePrinting)
	printer.SetProgress(30)
	poll(t, bridge, printer, "Core One L", 1)
	printer.SetState(StateAttention)
	poll(t, bridge, printer, "Core One L", 1)
	printer.SetState(StatePrinting)
	printer.SetProgress(35)
	poll(t, bridge, printer, "Core One L", 1)
	if n := countPendingSnapshots(t, bridge, "test-printer-id"); n != 2 {
		t.Fatalf("before restart: expected 2 pending snapshots, got %d", n)
	}
	bridge.mutex.RLock()
	startBefore := bridge.printStartTime["test-printer-id"]
	bridge.mutex.RUnlock()

	// Restart: a new bridge on the same database. NewFilamentBridge runs
	// ReconcileActiveSessions, which restores the in-memory print state.
	if err := bridge.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	restarted, err := NewFilamentBridge(nil)
	if err != nil {
		t.Fatalf("NewFilamentBridge after restart: %v", err)
	}
	t.Cleanup(func() { restarted.Close() })
	config, err := LoadConfig(restarted)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	config.SpoolmanURL = spoolman.URL()
	config.PrusaLinkTimeout = 5
	config.PrusaLinkFileDownloadTimeout = 10
	restarted.UpdateConfig(config)

	restarted.mutex.RLock()
	startAfter := restarted.printStartTime["test-printer-id"]
	restarted.mutex.RUnlock()
	if !startAfter.Equal(startBefore) {
		t.Fatalf("print start time not restored exactly: before %v, after %v", startBefore, startAfter)
	}
	snaps, err := restarted.GetCurrentPrintPendingSnapshots("test-printer-id")
	if err != nil {
		t.Fatalf("GetCurrentPrintPendingSnapshots: %v", err)
	}
	if len(snaps) != 2 {
		t.Errorf("after restart: live view should still show 2 snapshots, got %d", len(snaps))
	}

	// The first poll after the restart must not treat the print as new.
	printer.SetProgress(60)
	poll(t, restarted, printer, "Core One L", 1)
	if n := countPendingSnapshots(t, restarted, "test-printer-id"); n != 2 {
		t.Fatalf("first poll after restart: expected 2 pending snapshots kept, got %d", n)
	}

	// The print finishes: both snapshots attach to its history record.
	printer.SetState(StateFinished)
	poll(t, restarted, printer, "Core One L", 1)
	if n := countPendingSnapshots(t, restarted, "test-printer-id"); n != 0 {
		t.Errorf("after finish: expected 0 pending snapshots, got %d", n)
	}
	var attached int
	if err := restarted.db.QueryRow(`
		SELECT COUNT(*) FROM print_attachments
		WHERE file_type = 'camera' AND (label LIKE 'Attention%' OR label LIKE 'Resumed%')`).Scan(&attached); err != nil {
		t.Fatalf("count attachments: %v", err)
	}
	if attached != 2 {
		t.Errorf("expected the Attention and Resumed snapshots attached to the print, got %d", attached)
	}
}

// TestPendingSnapshots_OrphansPurgedOnNewPrint verifies that rows (and image files)
// left over from earlier prints are removed when a new print starts.
func TestPendingSnapshots_OrphansPurgedOnNewPrint(t *testing.T) {
	const spoolID = 72
	bridge, printer, _ := setupBridgeWithMocks(t, map[int]float64{spoolID: 500.0})
	gcodeDir := t.TempDir()
	t.Setenv("THE_MOMENT_GCODE_PATH", gcodeDir)
	if err := bridge.SetToolheadMapping("Core One L", 0, spoolID); err != nil {
		t.Fatalf("SetToolheadMapping: %v", err)
	}
	printer.SetGcodeUsage(map[int]float64{0: 10.0})

	// Plant orphans from two earlier "prints", as found in production.
	bridge.savePendingSnapshot("test-printer-id", time.Now().Add(-96*time.Hour), "attention", "Attention @ 0%", minimalJPEG)
	bridge.savePendingSnapshot("test-printer-id", time.Now().Add(-24*time.Hour), "attention", "Attention @ 0%", minimalJPEG)
	// An orphan on another printer must be left alone.
	bridge.savePendingSnapshot("other-printer", time.Now().Add(-24*time.Hour), "attention", "Attention @ 0%", minimalJPEG)

	var orphanFiles []string
	rows, err := bridge.db.Query(`SELECT file_path FROM pending_print_snapshots WHERE printer_id = ?`, "test-printer-id")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for rows.Next() {
		var p string
		_ = rows.Scan(&p)
		orphanFiles = append(orphanFiles, filepath.Join(gcodeDir, p))
	}
	rows.Close()
	if len(orphanFiles) != 2 {
		t.Fatalf("expected 2 planted orphans, got %d", len(orphanFiles))
	}

	printer.SetJobID(320)
	printer.SetState(StatePrinting)
	printer.SetProgress(5)
	poll(t, bridge, printer, "Core One L", 1)

	if n := countPendingSnapshots(t, bridge, "test-printer-id"); n != 0 {
		t.Errorf("expected orphans purged on new print, %d rows remain", n)
	}
	for _, f := range orphanFiles {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("orphan file not removed: %s (stat err: %v)", f, err)
		}
	}
	if n := countPendingSnapshots(t, bridge, "other-printer"); n != 1 {
		t.Errorf("other printer's rows must be untouched: want 1, got %d", n)
	}
}

// TestActiveSnapshotsEndpoint_OnlyCurrentPrint verifies the live view shows only the
// current print's snapshots, never another print's — even when the current print has
// none of its own (the case that surfaced the bug).
func TestActiveSnapshotsEndpoint_OnlyCurrentPrint(t *testing.T) {
	const spoolID = 73
	bridge, printer, _ := setupBridgeWithMocks(t, map[int]float64{spoolID: 500.0})
	t.Setenv("THE_MOMENT_GCODE_PATH", t.TempDir())
	if err := bridge.SetToolheadMapping("Core One L", 0, spoolID); err != nil {
		t.Fatalf("SetToolheadMapping: %v", err)
	}
	printer.SetGcodeUsage(map[int]float64{0: 10.0})

	gin.SetMode(gin.TestMode)
	r := gin.New()
	ws := &WebServer{bridge: bridge}
	r.GET("/api/printers/:id/active-snapshots", ws.activePrinterSnapshotsHandler)

	get := func() []map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/printers/test-printer-id/active-snapshots", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		var body struct {
			Snapshots []map[string]any `json:"snapshots"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body.Snapshots
	}

	// Current print with no snapshots of its own.
	printer.SetJobID(330)
	printer.SetState(StatePrinting)
	printer.SetProgress(20)
	poll(t, bridge, printer, "Core One L", 1)

	// An orphan from some other print appears (newer start time than ours, so the old
	// MAX(print_start_time) query would have picked it).
	bridge.savePendingSnapshot("test-printer-id", time.Now().Add(time.Hour), "attention", "Attention @ 0%", minimalJPEG)

	if snaps := get(); len(snaps) != 0 {
		t.Errorf("current print has no snapshots; endpoint returned %d: %v", len(snaps), snaps)
	}

	// A snapshot that does belong to the current print is shown.
	bridge.mutex.RLock()
	start := bridge.printStartTime["test-printer-id"]
	bridge.mutex.RUnlock()
	bridge.savePendingSnapshot("test-printer-id", start, "progress_25", "25% progress", minimalJPEG)
	snaps := get()
	if len(snaps) != 1 || snaps[0]["label"] != "25% progress" {
		t.Errorf("want exactly the current print's snapshot, got %v", snaps)
	}

	// No print tracked → nothing shown.
	printer.SetState(StateFinished)
	poll(t, bridge, printer, "Core One L", 1)
	if snaps := get(); len(snaps) != 0 {
		t.Errorf("no active print; endpoint returned %d: %v", len(snaps), snaps)
	}
}

// TestBambu_PrePrintPauseNotOrphaned is the Bambu equivalent of the PrusaLink case:
// the start-time bug lived in shared monitor state, not in PrusaLink specifically.
func TestBambu_PrePrintPauseNotOrphaned(t *testing.T) {
	const spoolID = 74
	cameraServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(minimalJPEG) //nolint:errcheck
	}))
	t.Cleanup(cameraServer.Close)

	bridge, mock, _ := setupBridgeWithBambuMock(t, map[int]float64{spoolID: 500.0})
	t.Setenv("THE_MOMENT_GCODE_PATH", t.TempDir())
	if err := bridge.SetToolheadMapping("X1C Printer", 0, spoolID); err != nil {
		t.Fatalf("SetToolheadMapping: %v", err)
	}
	cfg := bambuTestConfig("X1C Printer", 1)
	cfg.CameraSnapshotURL = cameraServer.URL + "/snapshot.jpg"

	// Print A runs to completion.
	mock.SetState("RUNNING")
	mock.SetGcodeFile("a.gcode")
	mock.SetProgress(50)
	pollBambu(t, bridge, "test-bambu", cfg)
	mock.SetState("FINISH")
	mock.SetFilamentTotal(10)
	pollBambu(t, bridge, "test-bambu", cfg)

	// Print B is paused before the monitor sees it running.
	mock.SetGcodeFile("b.gcode")
	mock.SetProgress(0)
	mock.SetState("PAUSE")
	pollBambu(t, bridge, "test-bambu", cfg)

	if n := countPendingSnapshots(t, bridge, "test-bambu"); n != 0 {
		t.Errorf("pre-print PAUSE: expected 0 pending snapshots (would be orphaned under print A), got %d", n)
	}

	// Print B's first RUNNING poll must not record an unpaired "Resumed" snapshot.
	mock.SetState("RUNNING")
	mock.SetProgress(5)
	pollBambu(t, bridge, "test-bambu", cfg)
	if n := countPendingSnapshots(t, bridge, "test-bambu"); n != 0 {
		t.Errorf("first RUNNING poll after pre-print PAUSE: expected 0 pending snapshots, got %d", n)
	}

	// A pause during the tracked print still records both snapshots.
	mock.SetState("PAUSE")
	pollBambu(t, bridge, "test-bambu", cfg)
	mock.SetState("RUNNING")
	pollBambu(t, bridge, "test-bambu", cfg)
	if n := countPendingSnapshots(t, bridge, "test-bambu"); n != 2 {
		t.Errorf("mid-print pause and resume: expected 2 pending snapshots, got %d", n)
	}
}
