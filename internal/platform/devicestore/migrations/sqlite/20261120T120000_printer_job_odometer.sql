-- The extruder's odometer at the start of a print and at its last reading
-- while it ran, in millimetres of filament, and the odometer's id at the
-- start. The difference is the filament the print used, measured by the
-- machine. The id changes when the machine resets its odometers, and a
-- reading under another id is not compared with the start. Each is NULL when
-- the machine reported no odometer, and the start is NULL for a print this
-- server first saw after it had begun.
ALTER TABLE printer_job ADD COLUMN odometer_start REAL;
ALTER TABLE printer_job ADD COLUMN odometer_end REAL;
ALTER TABLE printer_job ADD COLUMN odometer_id INTEGER;
