#!/bin/sh
set -e -x

# Delta restore turns itself off when it cannot do its job, and the restore then follows the usual
# rules. This covers both cases: an empty destination directory, and a backup that carries no file
# checksums to compare against.

. /tmp/tests/test_functions/pg_compat.sh

# TEMPORARY: while the feature is being worked on, run this test on PostgreSQL 18 only.
if [ "${PG_MAJOR}" != "18" ]; then
  echo "SKIP: temporarily limited to PostgreSQL 18"
  exit 77
fi
. /tmp/tests/test_functions/prepare_config.sh
prepare_config "/tmp/configs/delta_restore_fallback_test_config.json"

initdb ${PGDATA}

echo "archive_mode = on" >> ${PGDATA}/postgresql.conf
echo "archive_command = '/usr/bin/timeout 600 wal-g --config=${TMP_CONFIG} wal-push %p'" >> ${PGDATA}/postgresql.conf
echo "archive_timeout = 600" >> ${PGDATA}/postgresql.conf

pg_ctl -D ${PGDATA} -w start

wal-g --config=${TMP_CONFIG} st rm / --target=all || true

pgbench -i -s 5 postgres
dump_all /tmp/dump1
pgbench -c 2 -T 100000000 -S &
sleep 1

wal-g --config=${TMP_CONFIG} backup-push ${PGDATA}

# A backup without file metadata has nothing to compare against.
WALG_WITHOUT_FILES_METADATA=true wal-g --config=${TMP_CONFIG} backup-push ${PGDATA}
NO_METADATA_BACKUP=$(wal-g --config=${TMP_CONFIG} backup-list | tail -1 | cut -f 1 -d " ")

/tmp/scripts/drop_pg.sh

# An empty destination directory: delta restore is pointless there, and must behave exactly like a
# regular restore.
wal-g --config=${TMP_CONFIG} backup-fetch ${PGDATA} LATEST --delta-restore 2>&1 | tee /tmp/delta_restore_empty.log

if ! grep -q "is empty, doing a regular restore" /tmp/delta_restore_empty.log; then
  echo "Error: delta restore did not fall back to a regular restore for an empty directory"
  exit 1
fi

echo "restore_command = 'echo \"WAL file restoration: %f, %p\"&& wal-g --config=${TMP_CONFIG} wal-fetch \"%f\" \"%p\"'" | write_recovery_settings

pg_ctl -D ${PGDATA} -w start
/tmp/scripts/wait_while_pg_not_ready.sh
dump_all /tmp/dump2

compare_dumps /tmp/dump1 /tmp/dump2

echo "Delta restore into an empty directory success!!!!!!"

# A backup with no checksums to compare against: delta restore has to give up, and because the
# directory is not empty the restore is then refused rather than silently overwriting it.
pg_ctl -D ${PGDATA} -w -m immediate stop

set +e
wal-g --config=${TMP_CONFIG} backup-fetch ${PGDATA} ${NO_METADATA_BACKUP} --delta-restore > /tmp/delta_restore_no_checksums.log 2>&1
EXIT_STATUS=$?
set -e

cat /tmp/delta_restore_no_checksums.log

if [ "$EXIT_STATUS" -eq 0 ]; then
  echo "Error: delta restore of a backup without checksums should not have succeeded"
  exit 1
fi
if ! grep -q "must be empty" /tmp/delta_restore_no_checksums.log; then
  echo "Error: delta restore of a backup without checksums failed for the wrong reason"
  exit 1
fi

echo "Delta restore fallback success!!!!!!"

/tmp/scripts/drop_pg.sh
rm ${TMP_CONFIG}
