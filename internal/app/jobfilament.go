package app

// The filament a print used, measured by the machine, and what the slicer said
// it would use.
//
// A Prusa machine reports no filament for a print. It does report its
// extruder's odometer, the filament the first head has fed since its odometers
// were last reset. The odometer when the print began, taken off it later,
// is what the print used. The first reading counts only for a print that has
// just begun, because a reading taken part way would leave out what came
// before it. The rules that keep the start and the end are the upsert's
// (devicestore/printerjob.go).
//
// The slicer's figures come from the file itself, read as this server sends
// it to the printer (rememberPrintFile), because a printer tells this server
// nothing about what is inside its files.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/prusa3d/prusa-connect/internal/platform/devicestore"
	"github.com/prusa3d/prusa-connect/internal/printfile"
	"github.com/prusa3d/prusa-connect/internal/transport/http/httpapi"
	"github.com/prusa3d/prusa-connect/sdk/connectorapi"
)

//nolint:gochecknoglobals // two fixed paths, read-only after init.
var (
	odometerPath   = connectorapi.ToolPath(connectorapi.HeadID(0), connectorapi.OdometerE)
	odometerIDPath = connectorapi.ToolPath(connectorapi.HeadID(0), connectorapi.OdometerID)
)

const (
	// justBegunSeconds is how long a print may have printed when it is first
	// seen and still be measured from that reading. A machine that reports
	// the time its print has printed is held to it.
	justBegunSeconds = 300
	// justBegunPercent is the same for a machine that reports no printing
	// time, read off its progress.
	justBegunPercent = 1
	// printFileReadBudget bounds reading the slicer's figures out of a file
	// that has just been sent.
	printFileReadBudget = 10 * time.Second
)

// withOdometer puts the odometer reading of this write on a job row: the end
// on every write, and the start too on a write about a print that has just
// begun. A machine that reports no odometer id is given id 0, so its readings
// still compare with each other.
func withOdometer(row devicestore.PrinterJob, state map[string]json.RawMessage, facts jobFacts) devicestore.PrinterJob {
	value, known := jobCount(state[odometerPath])
	if !known {
		return row
	}

	var id int64
	if reported, idKnown := jobCount(state[odometerIDPath]); idKnown {
		id = int64(reported)
	}

	row.OdometerEnd, row.OdometerID = &value, &id

	if facts.underWay() && justBegun(facts) {
		start := value
		row.OdometerStart = &start
	}

	return row
}

// justBegun reports whether the reading is from the first moments of the
// print. A machine can carry the printing time of the print before it for a
// moment after the job number changes; that time is long, so such a reading is
// refused here and the next one is taken.
func justBegun(facts jobFacts) bool {
	if facts.printingKnown {
		return facts.printing <= justBegunSeconds
	}

	return facts.progress <= justBegunPercent
}

// renderJob is one stored job on the wire, with the slicer's figures for its
// file when this server sent that file before the print began. A store that
// cannot answer leaves the figures out: they are an extra on the print.
func (d *devices) renderJob(ctx context.Context, job devicestore.PrinterJob) httpapi.DeviceJob {
	out := jobRender(job)

	files, ok := d.records.(slicerFigures)
	if !ok || job.Path == "" {
		return out
	}

	file, found, err := files.PrintFileAt(ctx, job.DeviceID, job.Path, job.DisplayName)
	if err != nil {
		d.log.DebugContext(ctx, "the slicer's figures for a print could not be read",
			"device_id", job.DeviceID, "job_id", job.JobID, "error", err.Error())

		return out
	}

	if !found || file.Sent.After(job.Started) {
		return out
	}

	out.Slicer = &httpapi.JobSlicer{
		FilamentMM:       file.FilamentMM,
		FilamentG:        file.FilamentG,
		EstimatedSeconds: file.EstimatedSeconds,
		Material:         file.Material,
	}

	return out
}

// slicerFigures is the part of the record store that keeps the slicer's figures
// of the files this server sent. It is asked for and not required, so a
// record store without it still serves every print.
type slicerFigures interface {
	SavePrintFile(ctx context.Context, file devicestore.PrintFile) error
	PrintFileAt(ctx context.Context, deviceID, path, name string) (devicestore.PrintFile, bool, error)
}

// rememberPrintFile reads the slicer's figures out of a file that has just
// reached a printer, and keeps them for the prints of that file. A file that
// says nothing about filament or time is not kept, and a failure is logged and
// changes nothing about the transfer.
func (d *devices) rememberPrintFile(ctx context.Context, id, path string, content connectorapi.Content) {
	files, ok := d.records.(slicerFigures)
	if !ok || content == nil || content.Size() <= 0 {
		return
	}

	reading, done := context.WithTimeout(context.WithoutCancel(ctx), printFileReadBudget)
	defer done()

	found := printfile.Read(reading, contentReaderAt{ctx: reading, content: content}, content.Size()).Details
	if found.FilamentMM <= 0 && found.FilamentG <= 0 && found.EstimatedSeconds <= 0 {
		return
	}

	err := files.SavePrintFile(reading, devicestore.PrintFile{
		DeviceID:         id,
		Path:             path,
		Size:             content.Size(),
		FilamentMM:       found.FilamentMM,
		FilamentG:        found.FilamentG,
		EstimatedSeconds: found.EstimatedSeconds,
		Material:         found.Material,
		Sent:             time.Now(),
	})
	if err != nil {
		d.log.WarnContext(ctx, "the slicer's figures for a file sent to a printer could not be kept",
			"device_id", id, "path", path, "error", err.Error())
	}
}

// contentReaderAt reads a transfer's content at any offset, opening it there
// for each read.
type contentReaderAt struct {
	ctx     context.Context //nolint:containedctx // the reader serves one bounded read of one file.
	content connectorapi.Content
}

func (r contentReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.content.Size() {
		return 0, io.EOF
	}

	reader, err := r.content.Open(r.ctx, off)
	if err != nil {
		return 0, err
	}

	defer func() { _ = reader.Close() }()

	n, err := io.ReadFull(reader, p)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}

	return n, err
}
