#!/bin/sh
set -e -x

. /tmp/tests/test_functions/pg_compat.sh
. /tmp/tests/test_functions/prepare_config.sh
prepare_config "/tmp/configs/delta_restore_test_config.json"

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

# Stop the cluster but keep the data directory: that is what delta restore works against.
pg_ctl -D ${PGDATA} -w -m immediate stop

# Make the data directory diverge from the backup: scribble over a few files that are in it, and
# add one that is not.
for FILE in $(find ${PGDATA}/base -type f -size +8k | head -3); do
  dd if=/dev/urandom of="${FILE}" bs=8192 count=1 conv=notrunc
done
echo "this file is not part of the backup" > ${PGDATA}/leftover_file

wal-g --config=${TMP_CONFIG} backup-fetch ${PGDATA} LATEST --delta-restore 2>&1 | tee /tmp/delta_restore.log

# Files that still matched must have been kept rather than downloaded again.
if ! grep -q "files match the backup and are kept" /tmp/delta_restore.log; then
  echo "Error: delta restore did not report any preserved files"
  exit 1
fi
if grep -q "0 files match the backup and are kept" /tmp/delta_restore.log; then
  echo "Error: delta restore preserved nothing, it fetched the whole backup"
  exit 1
fi

# The file that is not in the backup must have been reported.
if ! grep -q "would remove invalid file .*leftover_file" /tmp/delta_restore.log; then
  echo "Error: delta restore did not report the file that is not part of the backup"
  exit 1
fi

echo "restore_command = 'echo \"WAL file restoration: %f, %p\"&& wal-g --config=${TMP_CONFIG} wal-fetch \"%f\" \"%p\"'" | write_recovery_settings

pg_ctl -D ${PGDATA} -w start
/tmp/scripts/wait_while_pg_not_ready.sh
dump_all /tmp/dump2

compare_dumps /tmp/dump1 /tmp/dump2

psql -f /tmp/scripts/amcheck.sql -v "ON_ERROR_STOP=1" postgres

echo "Delta restore success!!!!!!"

/tmp/scripts/drop_pg.sh
rm ${TMP_CONFIG}
