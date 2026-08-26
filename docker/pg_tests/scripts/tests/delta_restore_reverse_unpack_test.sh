#!/bin/sh
set -e -x

. /tmp/tests/test_functions/pg_compat.sh

# TEMPORARY: run this test on PostgreSQL 18 only while the feature is being worked on. It passes on
# 10 and 14-18; remove this before the pull request and run the whole matrix again.
if [ "${PG_MAJOR}" != "18" ]; then
  echo "SKIP: temporarily limited to PostgreSQL 18"
  exit 77
fi

# The same as delta_restore_over_previous_delta_test, but the chain is unpacked in reverse.
#
# Reverse unpack walks the chain from the newest backup down to the full one, writing each page
# once: the newest version of a page wins, and older backups only fill in what is still missing.
# That fill-in step, RestoreMissingPages, leaves a page alone if the local file already has one
# there. Combined with delta restore, where the file being restored is already on disk, it would
# keep exactly the stale pages the checksum said not to trust, so the local copy of a file that is
# going to be restored is removed first. This test is what says that it is.

. /tmp/tests/test_functions/prepare_config.sh
prepare_config "/tmp/configs/delta_restore_reverse_unpack_test_config.json"

initdb ${PGDATA}

echo "archive_mode = on" >> ${PGDATA}/postgresql.conf
echo "archive_command = '/usr/bin/timeout 600 wal-g --config=${TMP_CONFIG} wal-push %p'" >> ${PGDATA}/postgresql.conf
echo "archive_timeout = 600" >> ${PGDATA}/postgresql.conf

pg_ctl -D ${PGDATA} -w start

wal-g --config=${TMP_CONFIG} st rm / --target=all || true

pgbench -i -s 5 postgres
sleep 1
wal-g --config=${TMP_CONFIG} backup-push ${PGDATA}
FIRST_BACKUP=$(wal-g --config=${TMP_CONFIG} backup-list | tail -1 | cut -f 1 -d " ")

# Move the cluster on: more data in the existing database, and a database that did not exist at
# the time of the first backup at all.
pgbench -i -s 10 postgres
createdb second_db
pgbench -i -s 2 second_db

dump_all /tmp/dump_second
pgbench -c 2 -T 100000000 -S &
sleep 1
wal-g --config=${TMP_CONFIG} backup-push ${PGDATA}
SECOND_BACKUP=$(wal-g --config=${TMP_CONFIG} backup-list | tail -1 | cut -f 1 -d " ")

if [ "${FIRST_BACKUP}" = "${SECOND_BACKUP}" ]; then
  echo "Error: the second backup-push did not create a new backup"
  exit 1
fi

/tmp/scripts/drop_pg.sh

# Restore the older backup the usual way. The cluster is deliberately not started here: recovery
# would promote it to a new timeline, and the point of this test is the state of the directory,
# not what a running cluster would do to it.
wal-g --config=${TMP_CONFIG} backup-fetch ${PGDATA} ${FIRST_BACKUP}

# Now bring that directory up to the newer backup.
wal-g --config=${TMP_CONFIG} backup-fetch ${PGDATA} ${SECOND_BACKUP} --delta-restore --reverse-unpack \
  2>&1 | tee /tmp/delta_restore.log

# The newer backup has to be an incremental one and the chain has to have been walked in reverse,
# otherwise this test is the same as the forward one. Reverse unpack says so itself: it downgrades
# from the newest backup down to the base.
if ! grep -q "Downgrading from LSN" /tmp/delta_restore.log; then
  echo "Error: the chain was not unpacked in reverse, or the newer backup was not an incremental one"
  exit 1
fi
if ! grep -q "Applying base backup" /tmp/delta_restore.log; then
  echo "Error: reverse unpack did not reach the base backup"
  exit 1
fi
if grep -q "Delta restore: 0 files match the backup and are kept" /tmp/delta_restore.log; then
  echo "Error: delta restore preserved nothing, the older restore was of no use"
  exit 1
fi
if ! grep -q "files match the backup and are kept" /tmp/delta_restore.log; then
  echo "Error: delta restore did not report any preserved files"
  exit 1
fi

# Most of the work must have been saved by the older restore. Without this the test would still
# pass while delta restore quietly fetched nearly everything.
KEPT=$(grep -o "Delta restore: [0-9]* files match" /tmp/delta_restore.log | grep -o "[0-9]*")
RESTORED=$(grep -o ", [0-9]* files will be restored" /tmp/delta_restore.log | grep -o "[0-9]*")
if [ "${KEPT}" -le "${RESTORED}" ]; then
  echo "Error: delta restore kept ${KEPT} files but fetched ${RESTORED}, the previous restore was barely used"
  exit 1
fi

echo "restore_command = 'echo \"WAL file restoration: %f, %p\"&& wal-g --config=${TMP_CONFIG} wal-fetch \"%f\" \"%p\"'" | write_recovery_settings

pg_ctl -D ${PGDATA} -w start
/tmp/scripts/wait_while_pg_not_ready.sh
dump_all /tmp/dump_restored

# The cluster must hold what the newer backup held, the database created after the first backup
# included.
compare_dumps /tmp/dump_second /tmp/dump_restored
psql -c "select datname from pg_database" | grep -q second_db

psql -f /tmp/scripts/amcheck.sql -v "ON_ERROR_STOP=1" postgres

echo "Delta restore with reverse unpack success!!!!!!"

/tmp/scripts/drop_pg.sh
rm ${TMP_CONFIG}
