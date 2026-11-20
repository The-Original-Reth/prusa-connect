-- What the slicer said about a file this server sent to a printer, read from
-- the file as it was sent: one row per device and path, the newest send
-- winning. A print of that path reports these figures, so a client has the
-- slicer's estimate without reading the file off the printer. A row goes with
-- its device.
CREATE TABLE print_file (
    device_id         TEXT NOT NULL
                      REFERENCES authz_device (device_id) ON DELETE CASCADE,
    path              TEXT NOT NULL,
    size              INTEGER NOT NULL DEFAULT 0,
    filament_mm       REAL NOT NULL DEFAULT 0,
    filament_g        REAL NOT NULL DEFAULT 0,
    estimated_seconds INTEGER NOT NULL DEFAULT 0,
    material          TEXT NOT NULL DEFAULT '',
    sent              TIMESTAMP NOT NULL,
    PRIMARY KEY (device_id, path)
);
