package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prusa3d/prusa-connect/internal/platform/devicestore"
)

// TestAJobRowCarriesTheOdometer reads the odometer onto a job row: the end on
// every write, the start only for a print that has just begun, and nothing for
// a machine that reports no odometer.
func TestAJobRowCarriesTheOdometer(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		tree       map[string]string
		end, start *float64
		id         int64
	}{
		{
			name: "a print that has just begun is measured from here",
			tree: map[string]string{"/state": `"PRINTING"`, "/job/id": `506`, "/job/time_printing": `12`,
				"/tools/0/odometer_e": `1000.5`, "/tools/0/odometer_id": `3`},
			end: ptrTo(1000.5), start: ptrTo(1000.5), id: 3,
		},
		{
			name: "a first report carrying the print before's time is not a start",
			tree: map[string]string{"/state": `"PRINTING"`, "/job/id": `506`, "/job/time_printing": `29000`,
				"/tools/0/odometer_e": `1000.5`, "/tools/0/odometer_id": `3`},
			end: ptrTo(1000.5), id: 3,
		},
		{
			name: "a machine with no printing time is read by its progress",
			tree: map[string]string{"/state": `"PRINTING"`, "/job/id": `506`, "/job/progress": `0`,
				"/tools/0/odometer_e": `40`},
			end: ptrTo(40.0), start: ptrTo(40.0), id: 0,
		},
		{
			name: "a print that has ended carries its last reading and no start",
			tree: map[string]string{"/state": `"FINISHED"`, "/job/id": `506`, "/job/time_printing": `5`,
				"/tools/0/odometer_e": `1800`, "/tools/0/odometer_id": `3`},
			end: ptrTo(1800.0), id: 3,
		},
		{
			name: "a machine with no odometer",
			tree: map[string]string{"/state": `"PRINTING"`, "/job/id": `506`, "/job/time_printing": `5`},
		},
	}

	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			t.Parallel()

			tree := treeOf(one.tree)
			facts := jobFactsOf(tree)
			record := devicestore.DeviceRecord{DeviceID: "p-alpha", LastSeen: startedAtInApp, State: tree}

			row, held := jobRowOf(record, facts, "print-1")
			require.True(t, held)

			row = withOdometer(row, tree, facts)

			require.Equal(t, one.end, row.OdometerEnd)
			require.Equal(t, one.start, row.OdometerStart)

			if one.end != nil {
				require.Equal(t, one.id, *row.OdometerID)
			}
		})
	}
}

func ptrTo(value float64) *float64 { return &value }

// TestARenderedJobSaysItsMeasuredFilament puts the measured filament on the
// wire, and leaves it out for a print with no start on the odometer.
func TestARenderedJobSaysItsMeasuredFilament(t *testing.T) {
	t.Parallel()

	job := devicestore.PrinterJob{JobID: "print-1", DeviceID: "p-alpha", Started: startedAtInApp,
		OdometerStart: ptrTo(100), OdometerEnd: ptrTo(350.25)}

	rendered := jobRender(job)
	require.NotNil(t, rendered.FilamentUsedMM)
	require.InDelta(t, 250.25, *rendered.FilamentUsedMM, 0.001)

	job.OdometerStart = nil
	require.Nil(t, jobRender(job).FilamentUsedMM)
}

// slicerStore is a record store that keeps the slicer's figures and nothing
// else, for the two calls below.
type slicerStore struct {
	quietRecords

	files map[string]devicestore.PrintFile
}

func (s *slicerStore) SavePrintFile(_ context.Context, file devicestore.PrintFile) error {
	s.files[file.DeviceID+file.Path] = file

	return nil
}

func (s *slicerStore) PrintFileAt(_ context.Context, device, path, _ string) (devicestore.PrintFile, bool, error) {
	file, held := s.files[device+path]

	return file, held, nil
}

// bytesContent is a transfer's content held in memory.
type bytesContent []byte

func (b bytesContent) Size() int64 { return int64(len(b)) }

func (b bytesContent) Open(_ context.Context, offset int64) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(b[min(offset, int64(len(b))):])), nil
}

// TestASentFileGivesItsPrintsTheSlicersFigures keeps what the slicer wrote
// into a file as it is sent, and puts it on a print of that file that began
// after the send, but not on one that began before it.
func TestASentFileGivesItsPrintsTheSlicersFigures(t *testing.T) {
	t.Parallel()

	store := &slicerStore{files: map[string]devicestore.PrintFile{}}
	plane := &devices{records: store, log: slog.New(slog.DiscardHandler)}

	gcode := bytesContent("G28\nG1 X10\n; filament used [mm] = 1234.5\n; filament used [g] = 3.7\n" +
		"; estimated printing time (normal mode) = 1h 2m 3s\n; filament_type = PETG\n")
	plane.rememberPrintFile(t.Context(), "p-alpha", "/usb/frog.gcode", gcode)

	job := devicestore.PrinterJob{JobID: "print-1", DeviceID: "p-alpha", Path: "/usb/frog.gcode",
		Started: time.Now().Add(time.Minute)}

	rendered := plane.renderJob(t.Context(), job)
	require.NotNil(t, rendered.Slicer)
	require.InDelta(t, 1234.5, rendered.Slicer.FilamentMM, 0.001)
	require.InDelta(t, 3.7, rendered.Slicer.FilamentG, 0.001)
	require.Equal(t, int64(3723), rendered.Slicer.EstimatedSeconds)

	job.Started = time.Now().Add(-time.Hour)
	require.Nil(t, plane.renderJob(t.Context(), job).Slicer, "a print that began before the send took its figures")
}
