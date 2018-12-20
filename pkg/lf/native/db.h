/*
 * LF: Global Fully Replicated Key/Value Store
 * Copyright (C) 2018  ZeroTier, Inc.  https://www.zerotier.com/
 * 
 * Licensed under the terms of the MIT license (see LICENSE.txt).
 */

#ifndef ZT_LF_DB_H
#define ZT_LF_DB_H

#include "common.h"
#include "vector.h"
#include "mappedfile.h"

#ifdef ZTLF_SQLITE_INCLUDE
#include ZTLF_SQLITE_INCLUDE
#else
#include <sqlite3.h>
#endif

#define ZTLF_DB_GRAPH_NODE_LOCK_ARRAY_SIZE 197 /* prime to randomize lock distribution */

/**
 * Structure making up graph.bin
 * 
 * This packed structure tracks records' weights and links to other records by
 * graph node offset. It's stored in little endian format since most systems are
 * little endian and this therefore will usually give the best performance. The
 * graph.bin file is memory mapped for extremely fast traversal and weight
 * adjustment.
 */
ZTLF_PACKED_STRUCT(struct ZTLF_DB_GraphNode
{
	volatile uint64_t weightL;           /* least significant 64 bits of 80-bit weight */
	volatile uint16_t weightH;           /* most significant 16 bits of 80-bit weight */
	volatile uint64_t linkedCount;       /* number of nodes linking TO this one */
	uint8_t linkCount;                   /* size of linkedRecordGoff[] */
	volatile int64_t linkedRecordGoff[]; /* graph node offsets of linked records or -1 for holes (will be filled later) */
});

#define ZTLF_DB_MAX_GRAPH_NODE_SIZE (sizeof(struct ZTLF_DB_GraphNode) + (256 * sizeof(int64_t)))

/**
 * Write checkpoints no more often than once per hour.
 */
#define ZTLF_DB_MIN_CHECKPOINT_INTERVAL 3600000

/**
 * An instance of the LF database (C side)
 */
struct ZTLF_DB
{
	char path[PATH_MAX];

	sqlite3 *dbc;
	sqlite3_stmt *sSetConfig;
	sqlite3_stmt *sGetConfig;
	sqlite3_stmt *sAddRecord;
	sqlite3_stmt *sGetRecordCount;
	sqlite3_stmt *sGetDataSize;
	sqlite3_stmt *sGetAllRecords;
	sqlite3_stmt *sGetCompletedRecordCount;
	sqlite3_stmt *sGetCompletedRecordHashes;
	sqlite3_stmt *sGetLinkCandidates;
	sqlite3_stmt *sGetRecordByHash;
	sqlite3_stmt *sGetMaxRecordDoff;
	sqlite3_stmt *sGetMaxRecordGoff;
	sqlite3_stmt *sGetRecordGoffByHash;
	sqlite3_stmt *sGetRecordScoreByGoff;
	sqlite3_stmt *sGetRecordInfoByGoff;
	sqlite3_stmt *sGetDanglingLinks;
	sqlite3_stmt *sDeleteDanglingLinks;
	sqlite3_stmt *sDeleteWantedHash;
	sqlite3_stmt *sAddDanglingLink;
	sqlite3_stmt *sAddWantedHash;
	sqlite3_stmt *sAddHole;
	sqlite3_stmt *sFlagRecordWeightApplicationPending;
	sqlite3_stmt *sGetRecordsForWeightApplication;
	sqlite3_stmt *sGetHoles;
	sqlite3_stmt *sDeleteHole;
	sqlite3_stmt *sUpdatePendingHoleCount;
	sqlite3_stmt *sDeleteCompletedPending;
	sqlite3_stmt *sGetAnyPending;

	sqlite3_stmt *sGetMatching[16];

	volatile uint64_t lastCheckpoint;

	pthread_mutex_t dbLock;
	pthread_mutex_t graphNodeLocks[ZTLF_DB_GRAPH_NODE_LOCK_ARRAY_SIZE]; /* used to lock graph nodes by locking node lock goff % NODE_LOCK_ARRAY_SIZE */

	/* The write lock state of the RW locks for these memory mapped files is
	 * used to lock them in the case where the memory mapped file must be
	 * grown, since on most OSes this requires it to be unmapped and remapped.
	 * Otherwise only the read lock channel is used even when graph nodes are
	 * updated. To synchronize writes to graph nodes the graphNodeLocks mutex
	 * array is used. */
	struct ZTLF_MappedFile gf;
	pthread_rwlock_t gfLock;
	struct ZTLF_MappedFile df;
	pthread_rwlock_t dfLock;

	pthread_t graphThread;
	volatile bool graphThreadStarted;
	volatile bool running;
};

int ZTLF_DB_Open(struct ZTLF_DB *db,const char *path,char *errbuf,unsigned int errbufSize);

void ZTLF_DB_Close(struct ZTLF_DB *db);

int ZTLF_DB_PutRecord(
	struct ZTLF_DB *db,
	const void *rec,
	const unsigned int rsize,
	const void *id,
	const void *owner,
	const void *hash,
	const uint64_t ts,
	const uint64_t ttl,
	const uint32_t score,
	const void *changeOwner,
	const void *sel0,
	const void *sel1,
	const void *links,
	const unsigned int linkCount);

/* Function arguments: doff, dlen, ts, exp, id, owner, new_owner, least significant 64 bits of weight, most significant 64 bits of weight, arg */
void ZTLF_DB_GetMatching(struct ZTLF_DB *db,const void *id,const void *owner,const void *sel0,const void *sel1,int (*f)(int64_t,int64_t,uint64_t,uint64_t,void *,void *,void *,uint64_t,uint64_t,unsigned long),unsigned long arg);

/* Gets the data offset and data length of a record by its hash (returns length, sets doff). */
unsigned int ZTLF_DB_GetByHash(struct ZTLF_DB *db,const void *hash,uint64_t *doff);

/* Gets up to cnt hashes of records to which a new record should link, returning actual number of links written to lbuf. */
unsigned int ZTLF_DB_GetLinks(struct ZTLF_DB *db,void *const lbuf,const unsigned int cnt,const unsigned int desiredLinks);

/* Fill result pointer arguments with statistics about this database. */
void ZTLF_DB_Stats(struct ZTLF_DB *db,uint64_t *recordCount,uint64_t *dataSize);

/* Compute a CRC64 of all record hashes and their weights in deterministic order (for testing and consistency checking) */
uint64_t ZTLF_DB_CRC64(struct ZTLF_DB *db);

/* Returns nonzero if there are pending records (excluding those with dangling links). */
int ZTLF_DB_HasPending(struct ZTLF_DB *db);

static inline const char *ZTLF_DB_LastSqliteErrorMessage(struct ZTLF_DB *db) { return sqlite3_errmsg(db->dbc); }

static inline int ZTLF_DB_GetRecordData(struct ZTLF_DB *db,uint64_t doff,void *data,unsigned int dlen)
{
	pthread_rwlock_rdlock(&db->dfLock);
	void *const d = ZTLF_MappedFile_TryGet(&db->df,doff,(uintptr_t)dlen);
	if (d) {
		memcpy(data,d,dlen);
		pthread_rwlock_unlock(&db->dfLock);
		return 1;
	}
	pthread_rwlock_unlock(&db->dfLock);
	return 0;
}

#endif