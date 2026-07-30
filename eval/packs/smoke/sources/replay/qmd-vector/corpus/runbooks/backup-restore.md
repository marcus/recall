# Backup And Restore

## Restore Drill

Every quarter we restore the newest snapshot into a scratch volume and compare
checksums against the source. The drill is scheduled by a task so its rotation
is visible.

## Retention

Snapshots are kept for ninety days and are then deleted.
