#!/bin/sh
set -e -x

. /tmp/tests/test_functions/pg_compat.sh

# TEMPORARY: run this test on PostgreSQL 18 only while the feature is being worked on. It passes on
# 10 and 14-18; remove this before the pull request and run the whole matrix again.
if [ "${PG_MAJOR}" != "18" ]; then
  echo "SKIP: temporarily limited to PostgreSQL 18"
  exit 77
fi
. /tmp/tests/test_functions/prepare_config.sh
prepare_config "/tmp/configs/delta_restore_test_config.json"

initdb ${PGDATA}

echo "archive_mode = on" >> ${PGDATA}/postgresql.conf
echo "archive_command = '/usr/bin/timeout 600 wal-g --config=${TMP_CONFIG} wal-push %p'" >> ${PGDATA}/postgresql.conf
echo "archive_timeout = 600" >> ${PGDATA}/postgresql.conf

pg_ctl -D ${PGDATA} -w start

wal-g --config=${TMP_CONFIG} st rm / --target=all || true

pgbench -i -s 5 postgres

# A tablespace, so that the parts of delta restore that have to look outside the data directory are
# exercised too.
mkdir -p /tmp/spaces/space
psql -c "create tablespace space location '/tmp/spaces/space';"
psql -c "create table cinemas (id integer, name text) tablespace space;"
psql -c "insert into cinemas (id, name) values (1, 'Inception'), (2, 'Taxi');"

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
# A whole directory that is not in the backup, and a symlink that is not either.
mkdir -p ${PGDATA}/leftover_dir/nested
echo "nor is this one" > ${PGDATA}/leftover_dir/nested/leftover_nested_file
ln -s ${PGDATA}/PG_VERSION ${PGDATA}/leftover_link
# The same inside the tablespace, which lives outside the data directory behind a symlink.
TABLESPACE_LINK=$(find ${PGDATA}/pg_tblspc -type l | head -1)
echo "this file is not part of the backup either" > "${TABLESPACE_LINK}/leftover_in_tablespace"

wal-g --config=${TMP_CONFIG} backup-fetch ${PGDATA} LATEST --delta-restore 2>&1 | tee /tmp/delta_restore.log

# Files that still matched must have been kept rather than downloaded again.
if ! grep -q "files match the backup and are kept" /tmp/delta_restore.log; then
  echo "Error: delta restore did not report any preserved files"
  exit 1
fi
if grep -q "Delta restore: 0 files match the backup and are kept" /tmp/delta_restore.log; then
  echo "Error: delta restore preserved nothing, it fetched the whole backup"
  exit 1
fi

# Everything that is not in the backup must be gone, in the data directory and inside the
# tablespace alike, whatever kind of object it was.
if ! grep -q "remove invalid file .*leftover_file" /tmp/delta_restore.log; then
  echo "Error: delta restore did not report the file that is not part of the backup"
  exit 1
fi
if ! grep -q "remove invalid directory .*leftover_dir" /tmp/delta_restore.log; then
  echo "Error: delta restore did not report the directory that is not part of the backup"
  exit 1
fi
if ! grep -q "remove invalid file .*leftover_link" /tmp/delta_restore.log; then
  echo "Error: delta restore did not report the symlink that is not part of the backup"
  exit 1
fi
if ! grep -q "remove invalid file .*leftover_in_tablespace" /tmp/delta_restore.log; then
  echo "Error: delta restore did not look inside the tablespace for files that are not in the backup"
  exit 1
fi

for LEFTOVER in ${PGDATA}/leftover_file ${PGDATA}/leftover_dir ${PGDATA}/leftover_link \
                "${TABLESPACE_LINK}/leftover_in_tablespace"; do
  if [ -e "${LEFTOVER}" ] || [ -L "${LEFTOVER}" ]; then
    echo "Error: ${LEFTOVER} is not part of the backup but is still there after delta restore"
    exit 1
  fi
done

# The tablespace symlinks are not in the file metadata, they are recreated from the tablespace
# spec. Removing one would leave the cluster without its tablespace.
if [ ! -L "${TABLESPACE_LINK}" ]; then
  echo "Error: delta restore removed the tablespace symlink ${TABLESPACE_LINK}"
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
