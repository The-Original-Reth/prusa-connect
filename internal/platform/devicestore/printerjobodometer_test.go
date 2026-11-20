package devicestore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func ptr[T any](value T) *T { return &value }

// withReading is a running print carrying one odometer reading: the end, the
// id, and the start when start is true.
func withReading(job PrinterJob, value float64, id int64, start bool) PrinterJob {
	job.OdometerEnd, job.OdometerID = ptr(value), ptr(id)
	if start {
		job.OdometerStart = ptr(value)
	}

	return job
}

// TestAPrintMeasuresItsFilamentFromTheOdometer walks one print's odometer:
// the start is the first reading that carries one and stays, the end follows
// the readings under the start's id, a reading under another id or below the
// start is not this print's, and the end stops moving once the print ends.
func TestAPrintMeasuresItsFilamentFromTheOdometer(t *testing.T) {
	t.Parallel()

	forEachEngine(t, func(t *testing.T, e engine) {
		store := openFixture(t, e, migrated(t, e, e.OpenShared(t, 1)[0]))
		aRegisteredDevice(t, store, "p-alpha", "Alpha")

		running := aRunningJob("p-alpha", "506")
		read := func() PrinterJob {
			found, err := store.DeviceJobs(t.Context(), "p-alpha", currentAndLast)
			require.NoError(t, err)
			require.Len(t, found, 1)

			return found[0]
		}

		// First seen part way: no start, so nothing is measured.
		require.NoError(t, store.SaveDeviceJobs(t.Context(), withReading(running, 900, 4, false)))
		_, measured := read().FilamentUsed()
		require.False(t, measured, "a print with no odometer at its start was measured")

		// A later write carries a start: it is taken, with its id.
		require.NoError(t, store.SaveDeviceJobs(t.Context(), withReading(running, 1000, 4, true)))
		used, measured := read().FilamentUsed()
		require.True(t, measured)
		require.InDelta(t, 0, used, 0.001)

		// A start offered again is not taken: the first one stays.
		require.NoError(t, store.SaveDeviceJobs(t.Context(), withReading(running, 1500, 4, true)))
		used, _ = read().FilamentUsed()
		require.InDelta(t, 500, used, 0.001)

		// A reading after a reset (another id) or below the start is not this print's.
		require.NoError(t, store.SaveDeviceJobs(t.Context(), withReading(running, 20, 5, false)))
		require.NoError(t, store.SaveDeviceJobs(t.Context(), withReading(running, 990, 4, false)))
		used, _ = read().FilamentUsed()
		require.InDelta(t, 500, used, 0.001)

		// The write that ends the print carries the last reading, and the row is frozen after it.
		ended := withReading(running, 1800.5, 4, false)
		ended.Ended, ended.State, ended.Outcome = startedAt.Add(time.Hour), "FINISHED", "finished"
		require.NoError(t, store.SaveDeviceJobs(t.Context(), ended))

		restated := withReading(running, 2500, 4, false)
		restated.Ended = startedAt.Add(2 * time.Hour)
		require.NoError(t, store.SaveDeviceJobs(t.Context(), restated))

		used, _ = read().FilamentUsed()
		require.InDelta(t, 800.5, used, 0.001)
	})
}

// TestAPrintFileKeepsTheNewestSend writes the slicer's figures for one path
// twice and reads the newest back, whatever the case the printer names it in.
func TestAPrintFileKeepsTheNewestSend(t *testing.T) {
	t.Parallel()

	forEachEngine(t, func(t *testing.T, e engine) {
		store := openFixture(t, e, migrated(t, e, e.OpenShared(t, 1)[0]))
		aRegisteredDevice(t, store, "p-alpha", "Alpha")

		_, held, err := store.PrintFileAt(t.Context(), "p-alpha", "/usb/frog.bgcode")
		require.NoError(t, err)
		require.False(t, held)

		first := PrintFile{DeviceID: "p-alpha", Path: "/usb/frog.bgcode", Size: 10, FilamentMM: 1000,
			FilamentG: 3, EstimatedSeconds: 600, Material: "PLA", Sent: startedAt}
		require.NoError(t, store.SavePrintFile(t.Context(), first))

		second := first
		second.FilamentMM, second.Sent = 2000, startedAt.Add(time.Hour)
		require.NoError(t, store.SavePrintFile(t.Context(), second))

		found, held, err := store.PrintFileAt(t.Context(), "p-alpha", "/USB/FROG.BGCODE")
		require.NoError(t, err)
		require.True(t, held)
		require.InDelta(t, 2000, found.FilamentMM, 0.001)
		require.Equal(t, "PLA", found.Material)
		require.True(t, second.Sent.Equal(found.Sent), "sent came back as %s", found.Sent)
	})
}
