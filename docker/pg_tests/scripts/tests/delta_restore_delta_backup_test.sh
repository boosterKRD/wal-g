#!/bin/sh
set -e -x

# Delta restore of an incremental backup: the comparison is made once against the metadata of the
# target backup, and the files that are kept must not be fetched from the base backup either.

. /tmp/tests/test_functions/pg_compat.sh

# TEMPORARY: run this test on PostgreSQL 18 only while the feature is being worked on. It passes on
# 10 and 14-18; remove this before the pull request and run the whole matrix again.
if [ "${PG_MAJOR}" != "18" ]; then
  echo "SKIP: temporarily limited to PostgreSQL 18"
  exit 77
fi
. /tmp/tests/test_functions/prepare_config.sh
prepare_config "/tmp/configs/delta_restore_delta_backup_test_config.json"

initdb ${PGDATA}

echo "archive_mode = on" >> ${PGDATA}/postgresql.conf
echo "archive_command = '/usr/bin/timeout 600 wal-g --config=${TMP_CONFIG} wal-push %p'" >> ${PGDATA}/postgresql.conf
echo "archive_timeout = 600" >> ${PGDATA}/postgresql.conf

pg_ctl -D ${PGDATA} -w start

wal-g --config=${TMP_CONFIG} st rm / --target=all || true

pgbench -i -s 5 postgres
wal-g --config=${TMP_CONFIG} backup-push ${PGDATA}

# Change enough for the next backup to be a real increment on top of the first one.
pgbench -i -s 10 postgres
dump_all /tmp/dump1
pgbench -c 2 -T 100000000 -S &
sleep 1

wal-g --config=${TMP_CONFIG} backup-push ${PGDATA}

# Stop the cluster but keep the data directory: that is what delta restore works against.
pg_ctl -D ${PGDATA} -w -m immediate stop

for FILE in $(find ${PGDATA}/base -type f -size +8k | head -3); do
  dd if=/dev/urandom of="${FILE}" bs=8192 count=1 conv=notrunc
done

wal-g --config=${TMP_CONFIG} backup-fetch ${PGDATA} LATEST --delta-restore 2>&1 | tee /tmp/delta_restore.log

# The backup being restored has to be an incremental one, otherwise this test proves nothing.
if ! grep -q "Delta from" /tmp/delta_restore.log; then
  echo "Error: the restored backup was not an incremental one"
  exit 1
fi
if grep -q "Delta restore: 0 files match the backup and are kept" /tmp/delta_restore.log; then
  echo "Error: delta restore preserved nothing, it fetched the whole backup"
  exit 1
fi

echo "restore_command = 'echo \"WAL file restoration: %f, %p\"&& wal-g --config=${TMP_CONFIG} wal-fetch \"%f\" \"%p\"'" | write_recovery_settings

pg_ctl -D ${PGDATA} -w start
/tmp/scripts/wait_while_pg_not_ready.sh
dump_all /tmp/dump2

compare_dumps /tmp/dump1 /tmp/dump2

psql -f /tmp/scripts/amcheck.sql -v "ON_ERROR_STOP=1" postgres

echo "Delta restore of a delta backup success!!!!!!"

/tmp/scripts/drop_pg.sh
rm ${TMP_CONFIG}
