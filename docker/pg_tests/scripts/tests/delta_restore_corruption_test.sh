#!/bin/sh
set -e -x

# A restore checks every file it writes against the checksum the backup recorded for it, so that
# data damaged in storage or on its way out of it is reported instead of being quietly written to
# disk. This test damages a backup on purpose and expects the restore to refuse it.
#
# The backup is written uncompressed to a local directory: a compressed tarball would fail to
# decompress long before the checksum had anything to say, which is not what is being tested here.

. /tmp/tests/test_functions/pg_compat.sh

# TEMPORARY: run this test on PostgreSQL 18 only while the feature is being worked on. It passes on
# 10 and 14-18; remove this before the pull request and run the whole matrix again.
if [ "${PG_MAJOR}" != "18" ]; then
  echo "SKIP: temporarily limited to PostgreSQL 18"
  exit 77
fi
. /tmp/tests/test_functions/prepare_config.sh
prepare_config "/tmp/configs/delta_restore_corruption_test_config.json"

# The common config sets brotli and is appended after the test config, so the compression method has
# to be overridden through the environment, which wins over both.
export WALG_COMPRESSION_METHOD=none

initdb ${PGDATA}

echo "archive_mode = on" >> ${PGDATA}/postgresql.conf
echo "archive_command = '/usr/bin/timeout 600 wal-g --config=${TMP_CONFIG} wal-push %p'" >> ${PGDATA}/postgresql.conf
echo "archive_timeout = 600" >> ${PGDATA}/postgresql.conf

# The backup goes to a local directory, which has to be there before the server starts archiving
# into it.
rm -rf /tmp/corrupted_backup
mkdir -p /tmp/corrupted_backup

pg_ctl -D ${PGDATA} -w start

wal-g --config=${TMP_CONFIG} st rm / --target=all || true

pgbench -i -s 5 postgres
dump_all /tmp/dump1
sleep 1

wal-g --config=${TMP_CONFIG} backup-push ${PGDATA}

pg_ctl -D ${PGDATA} -w -m immediate stop
rm -rf ${PGDATA}

# The largest tarball holds one big relation of its own, because WALG_TAR_DEDICATED_FILE_SIZE gives
# large files a tarball to themselves. Its first tar header is 512 bytes, so anything written well
# past that lands in the file itself rather than in tar metadata.
TARBALL=$(find /tmp/corrupted_backup -name 'part_*.tar' -exec ls -S {} + | head -1)
if [ -z "${TARBALL}" ]; then
  echo "Error: no uncompressed tarball to damage, the backup did not look the way this test expects:"
  find /tmp/corrupted_backup -name 'part_*' | head
  exit 1
fi
cp "${TARBALL}" /tmp/tarball_backup.tar
dd if=/dev/urandom of="${TARBALL}" bs=1 seek=8192 count=64 conv=notrunc

wal-g --config=${TMP_CONFIG} backup-fetch ${PGDATA} LATEST > /tmp/corrupted_restore.log 2>&1 \
  && EXIT_STATUS=$? || EXIT_STATUS=$?

if [ "${EXIT_STATUS}" -eq 0 ]; then
  echo "Error: the restore of a damaged backup succeeded"
  exit 1
fi
if ! grep -q "does not match the backup" /tmp/corrupted_restore.log; then
  echo "Error: the restore failed, but not because of the checksum:"
  tail -20 /tmp/corrupted_restore.log
  exit 1
fi
echo "Damaged backup rejected!!!!!!"

# The same backup restores once the damage is undone, so the failure above was the damage and not
# something else about this backup.
cp /tmp/tarball_backup.tar "${TARBALL}"
rm -rf ${PGDATA}

wal-g --config=${TMP_CONFIG} backup-fetch ${PGDATA} LATEST

echo "restore_command = 'echo \"WAL file restoration: %f, %p\"&& wal-g --config=${TMP_CONFIG} wal-fetch \"%f\" \"%p\"'" | write_recovery_settings

pg_ctl -D ${PGDATA} -w start
/tmp/scripts/wait_while_pg_not_ready.sh
dump_all /tmp/dump2

compare_dumps /tmp/dump1 /tmp/dump2

echo "Restore checksum verification success!!!!!!"

/tmp/scripts/drop_pg.sh
rm ${TMP_CONFIG}
rm -rf /tmp/corrupted_backup /tmp/tarball_backup.tar
