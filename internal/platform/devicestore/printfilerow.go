package devicestore

// What the slicer said about a file this server sent to a printer. A print of
// that file reports the figures, so a client has the slicer's estimate without
// reading the file off the machine, which this server cannot do: a printer
// tells it the names, sizes and pictures of its files and nothing inside them.
//
// One row per device and path, the newest send winning. A row goes with its
// device through the foreign key.

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const (
	upsertPrintFileSQL = `INSERT INTO print_file
		(device_id, path, size, filament_mm, filament_g, estimated_seconds, material, sent)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (device_id, path) DO UPDATE SET
			size = excluded.size,
			filament_mm = excluded.filament_mm,
			filament_g = excluded.filament_g,
			estimated_seconds = excluded.estimated_seconds,
			material = excluded.material,
			sent = excluded.sent`

	// A machine can name the file it prints in another case than the path it
	// was sent to, so the path is compared without case.
	selectPrintFileSQL = `SELECT device_id, path, size, filament_mm, filament_g, estimated_seconds, material, sent
		FROM print_file
		WHERE device_id = ? AND LOWER(path) = LOWER(?)
		ORDER BY sent DESC
		LIMIT 1`
)

// PrintFile is what the slicer said about one file sent to one device.
type PrintFile struct {
	DeviceID string
	// Path is where the file was sent on the machine's storage.
	Path string
	// Size is the file's length in bytes as it was sent.
	Size int64
	// FilamentMM and FilamentG are the filament the slicer said the whole print
	// uses, and EstimatedSeconds the time it said the print takes. Zero is
	// "the slicer did not say".
	FilamentMM       float64
	FilamentG        float64
	EstimatedSeconds int64
	// Material is the filament type the file was sliced for, such as PLA.
	Material string
	// Sent is when the file reached the printer.
	Sent time.Time
}

// SavePrintFile writes what the slicer said about a file sent to a device,
// replacing what an earlier send to the same path left.
func (s *Store) SavePrintFile(ctx context.Context, file PrintFile) error {
	if file.DeviceID == "" || file.Path == "" {
		return errors.New("devicestore: a print file names no device or no path")
	}

	_, err := s.db.ExecContext(ctx, s.q(upsertPrintFileSQL),
		file.DeviceID, file.Path, file.Size, file.FilamentMM, file.FilamentG, file.EstimatedSeconds,
		file.Material, file.Sent.UTC())
	if err != nil {
		return failure("write a print file", err)
	}

	return nil
}

// PrintFileAt reads what the slicer said about the file at path on a device,
// and reports false when this server sent nothing there.
func (s *Store) PrintFileAt(ctx context.Context, deviceID, path string) (PrintFile, bool, error) {
	var file PrintFile

	err := s.db.QueryRowContext(ctx, s.q(selectPrintFileSQL), deviceID, path).Scan(
		&file.DeviceID, &file.Path, &file.Size, &file.FilamentMM, &file.FilamentG, &file.EstimatedSeconds,
		&file.Material, &file.Sent)
	if errors.Is(err, sql.ErrNoRows) {
		return PrintFile{}, false, nil
	}

	if err != nil {
		return PrintFile{}, false, failure("read a print file", err)
	}

	file.Sent = file.Sent.UTC()

	return file, true, nil
}
