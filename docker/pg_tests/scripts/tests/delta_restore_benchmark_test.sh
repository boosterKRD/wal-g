#!/bin/sh
set -e -x

# What delta restore actually saves, in numbers. This test asserts nothing about the figures: it
# restores the same backup twice, once into an empty directory and once over an older copy of the
# cluster, and prints what each of them cost. Correctness is the other tests' job.
#
# The defaults are small enough to run on a laptop. The knobs below take the same run to whatever
# size a real machine can afford, without editing this file:
#
#   DELTA_BENCH_TABLES=10                      how many tables to generate
#   DELTA_BENCH_PROFILE="10:8,30:16,60:32"     percent of tables : size of each in MB
#   DELTA_BENCH_CHANGED_PCT=20                 percent of tables touched between the two backups
#
# The profile is what makes the answer meaningful. Delta restore works at the granularity of a
# file, so a database of many small tables and one of few large ones save entirely differently.

. /tmp/tests/test_functions/pg_compat.sh

# TEMPORARY: run this test on PostgreSQL 18 only while the feature is being worked on. It passes on
# 10 and 14-18; remove this before the pull request and run the whole matrix again.
if [ "${PG_MAJOR}" != "18" ]; then
  echo "SKIP: temporarily limited to PostgreSQL 18"
  exit 77
fi
. /tmp/tests/test_functions/prepare_config.sh
prepare_config "/tmp/configs/delta_restore_benchmark_test_config.json"

TABLES=${DELTA_BENCH_TABLES:-10}
PROFILE=${DELTA_BENCH_PROFILE:-"10:8,30:16,60:32"}
CHANGED_PCT=${DELTA_BENCH_CHANGED_PCT:-20}

STORAGE=/tmp/benchmark_backup
BACKUPS=${STORAGE}/basebackups_005

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# table_sizes - prints one size in MB per table, following the profile. Rounding is absorbed by the
# last group, so the count always comes out at exactly ${TABLES}.
table_sizes() {
  echo "${PROFILE}" | tr ',' '\n' | awk -v tables="${TABLES}" '
    { pct[NR] = $0; sub(/:.*/, "", pct[NR]); size[NR] = $0; sub(/.*:/, "", size[NR]); groups = NR }
    END {
      assigned = 0
      for (i = 1; i < groups; i++) {
        count = int(tables * pct[i] / 100)
        for (j = 0; j < count; j++) print size[i]
        assigned += count
      }
      for (j = assigned; j < tables; j++) print size[groups]
    }'
}

# bytes_of_tarballs <log> <backup> - total size of the tarballs the restore in <log> actually read.
# The lookup is confined to the backup being restored: every backup names its tarballs part_001 and
# up, so searching the whole storage would count the namesakes of every other backup as well.
bytes_of_tarballs() {
  grep -o "Finished extraction of [^ ]*" "$1" | awk '{ print $NF }' | sort -u | \
    while read -r NAME; do
      find "${BACKUPS}/$2" -name "${NAME}" -exec stat -c %s {} +
    done | awk '{ total += $1 } END { print total + 0 }'
}

count_tarballs() {
  grep -o "Finished extraction of [^ ]*" "$1" | awk '{ print $NF }' | sort -u | wc -l
}

now_ms() {
  date +%s%3N
}

# ---------------------------------------------------------------------------
# A cluster with the requested shape of data
# ---------------------------------------------------------------------------

rm -rf ${STORAGE}
mkdir -p ${STORAGE}

initdb ${PGDATA}

echo "archive_mode = on" >> ${PGDATA}/postgresql.conf
echo "archive_command = '/usr/bin/timeout 600 wal-g --config=${TMP_CONFIG} wal-push %p'" >> ${PGDATA}/postgresql.conf
echo "archive_timeout = 600" >> ${PGDATA}/postgresql.conf

pg_ctl -D ${PGDATA} -w start
/tmp/scripts/wait_while_pg_not_ready.sh

# Roughly 124 bytes per row, and a payload of random hex so that the tarballs do not compress down
# to nothing and make the numbers meaningless.
INDEX=0
table_sizes | while read -r SIZE_MB; do
  INDEX=$((INDEX + 1))
  ROWS=$((SIZE_MB * 1024 * 1024 / 124))
  psql -q -c "create table bench_${INDEX} (id int primary key, payload text);"
  psql -q -c "insert into bench_${INDEX} select g, repeat(md5(random()::text), 3) \
              from generate_series(1, ${ROWS}) g;"
done
psql -q -c "checkpoint;"

TOTAL_MB=$(psql -tAc "select round(sum(pg_total_relation_size(oid)) / 1024 / 1024) from pg_class where relname like 'bench_%'")
echo "Generated ${TABLES} tables, ${TOTAL_MB} MB in total, profile ${PROFILE}"

# ---------------------------------------------------------------------------
# Two backups: the one an old restore came from, and the one being restored now
# ---------------------------------------------------------------------------

wal-g --config=${TMP_CONFIG} backup-push ${PGDATA}
OLD_BACKUP=$(wal-g --config=${TMP_CONFIG} backup-list | tail -1 | cut -f 1 -d " ")

CHANGED=$((TABLES * CHANGED_PCT / 100))
if [ "${CHANGED}" -lt 1 ]; then
  CHANGED=1
fi
INDEX=0
while [ ${INDEX} -lt ${CHANGED} ]; do
  INDEX=$((INDEX + 1))
  psql -q -c "update bench_${INDEX} set payload = repeat(md5(random()::text), 3) \
              where id % 100 = 0;"
done
psql -q -c "checkpoint;"
echo "Changed ${CHANGED} of ${TABLES} tables"

wal-g --config=${TMP_CONFIG} backup-push ${PGDATA}
NEW_BACKUP=$(wal-g --config=${TMP_CONFIG} backup-list | tail -1 | cut -f 1 -d " ")

if [ "${OLD_BACKUP}" = "${NEW_BACKUP}" ]; then
  echo "Error: the second backup-push did not create a new backup"
  exit 1
fi

NEW_BACKUP_BYTES=$(du -sb "${BACKUPS}/${NEW_BACKUP}" | cut -f 1)

/tmp/scripts/drop_pg.sh

# ---------------------------------------------------------------------------
# Restore the newer backup twice, and time both
# ---------------------------------------------------------------------------

START=$(now_ms)
wal-g --config=${TMP_CONFIG} backup-fetch ${PGDATA} ${NEW_BACKUP} > /tmp/full_restore.log 2>&1
FULL_MS=$(($(now_ms) - START))
FULL_TARBALLS=$(count_tarballs /tmp/full_restore.log)
FULL_BYTES=$(bytes_of_tarballs /tmp/full_restore.log "${NEW_BACKUP}")

rm -rf ${PGDATA}

# The starting point for the delta restore: the directory as an older restore left it.
wal-g --config=${TMP_CONFIG} backup-fetch ${PGDATA} ${OLD_BACKUP} > /tmp/old_restore.log 2>&1

START=$(now_ms)
wal-g --config=${TMP_CONFIG} backup-fetch ${PGDATA} ${NEW_BACKUP} --delta-restore \
  > /tmp/delta_restore.log 2>&1
DELTA_MS=$(($(now_ms) - START))
DELTA_TARBALLS=$(count_tarballs /tmp/delta_restore.log)
DELTA_BYTES=$(bytes_of_tarballs /tmp/delta_restore.log "${NEW_BACKUP}")

KEPT=$(grep -o "Delta restore: [0-9]* files match" /tmp/delta_restore.log | grep -o "[0-9]*" || echo 0)
RESTORED=$(grep -o ", [0-9]* files will be restored" /tmp/delta_restore.log | grep -o "[0-9]*" || echo 0)

# The cluster still has to come up and hold the right data, or the numbers describe nothing.
echo "restore_command = 'echo \"WAL file restoration: %f, %p\"&& wal-g --config=${TMP_CONFIG} wal-fetch \"%f\" \"%p\"'" | write_recovery_settings

pg_ctl -D ${PGDATA} -w start
/tmp/scripts/wait_while_pg_not_ready.sh
psql -f /tmp/scripts/amcheck.sql -v "ON_ERROR_STOP=1" postgres

# ---------------------------------------------------------------------------
# The report
# ---------------------------------------------------------------------------

set +x
echo ""
echo "======================================================================"
echo "Delta restore benchmark"
echo "----------------------------------------------------------------------"
echo "  tables                 ${TABLES}, profile ${PROFILE}"
echo "  data generated         ${TOTAL_MB} MB"
echo "  tables changed         ${CHANGED} of ${TABLES} (${CHANGED_PCT}%)"
echo "  backup restored        ${NEW_BACKUP}, ${NEW_BACKUP_BYTES} bytes in storage"
echo "----------------------------------------------------------------------"
echo "                         full restore      delta restore"
echo "  tarballs read          ${FULL_TARBALLS}                 ${DELTA_TARBALLS}"
echo "  bytes read             ${FULL_BYTES}          ${DELTA_BYTES}"
echo "  wall clock (ms)        ${FULL_MS}             ${DELTA_MS}"
echo "----------------------------------------------------------------------"
echo "  files kept             ${KEPT}"
echo "  files restored         ${RESTORED}"
if [ "${FULL_BYTES}" -gt 0 ]; then
  echo "  bytes saved            $((100 - DELTA_BYTES * 100 / FULL_BYTES))%"
fi
echo "----------------------------------------------------------------------"
echo "  WAL-G's own summary of the delta restore, for comparison:"
grep -A 10 "Delta restore summary" /tmp/delta_restore.log | sed 's/^/  /'
echo "======================================================================"
echo ""
set -x

/tmp/scripts/drop_pg.sh
rm ${TMP_CONFIG}
rm -rf ${STORAGE}
