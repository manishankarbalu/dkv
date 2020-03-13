package rocksdb

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/flipkart-incubator/dkv/internal/server/storage"
	"github.com/flipkart-incubator/dkv/pkg/serverpb"
	"github.com/tecbot/gorocksdb"
)

var (
	store            storage.KVStore
	changePropagator storage.ChangePropagator
	changeApplier    storage.ChangeApplier
)

const (
	createDBFolderIfMissing = true
	dbFolder                = "/tmp/rocksdb_storage_test"
	cacheSize               = 3 << 30
)

func TestMain(m *testing.M) {
	if kvs, err := openRocksDB(); err != nil {
		panic(err)
	} else {
		store, changePropagator, changeApplier = kvs, kvs, kvs
		res := m.Run()
		store.Close()
		os.Exit(res)
	}
}

func TestPutAndGet(t *testing.T) {
	numKeys := 10
	for i := 1; i <= numKeys; i++ {
		key, value := fmt.Sprintf("K%d", i), fmt.Sprintf("V%d", i)
		if err := store.Put([]byte(key), []byte(value)); err != nil {
			t.Fatalf("Unable to PUT. Key: %s, Value: %s, Error: %v", key, value, err)
		}
	}

	for i := 1; i <= numKeys; i++ {
		key, expectedValue := fmt.Sprintf("K%d", i), fmt.Sprintf("V%d", i)
		if readResults, err := store.Get([]byte(key)); err != nil {
			t.Fatalf("Unable to GET. Key: %s, Error: %v", key, err)
		} else {
			if string(readResults[0]) != expectedValue {
				t.Errorf("GET mismatch. Key: %s, Expected Value: %s, Actual Value: %s", key, expectedValue, readResults[0])
			}
		}
	}
}

func TestGetLatestChangeNumber(t *testing.T) {
	expNumTrxns := uint64(5)
	beforeChngNum, _ := changePropagator.GetLatestCommittedChangeNumber()
	putKeys(t, int(expNumTrxns), "aaKey", "aaVal")
	afterChngNum, _ := changePropagator.GetLatestCommittedChangeNumber()
	actNumTrxns := afterChngNum - beforeChngNum
	if expNumTrxns != actNumTrxns {
		t.Errorf("Mismatch in number of transactions. Expected: %d, Actual: %d", expNumTrxns, actNumTrxns)
	}
	beforeChngNum = afterChngNum
	getKeys(t, int(expNumTrxns), "aaKey", "aaVal")
	afterChngNum, _ = changePropagator.GetLatestCommittedChangeNumber()
	actNumTrxns = afterChngNum - beforeChngNum
	if actNumTrxns != 0 {
		t.Errorf("Expected no transactions to have occurred but found %d transactions", actNumTrxns)
	}
}

func TestLoadChanges(t *testing.T) {
	expNumTrxns, maxChngs := 3, 8
	keyPrefix, valPrefix := "bbKey", "bbVal"
	chngNum, _ := changePropagator.GetLatestCommittedChangeNumber()
	chngNum++ // due to possible previous transaction
	putKeys(t, expNumTrxns, keyPrefix, valPrefix)
	if chngs, err := changePropagator.LoadChanges(chngNum, maxChngs); err != nil {
		t.Fatal(err)
	} else {
		expNumChngs, actNumChngs := 3, len(chngs)
		if expNumChngs != actNumChngs {
			t.Errorf("Incorrect number of changes retrieved. Expected: %d, Actual: %d", expNumChngs, actNumChngs)
		}
		firstChngNum := chngs[0].ChangeNumber
		if firstChngNum != chngNum {
			t.Errorf("Expected first change number to be %d but it is %d", chngNum, firstChngNum)
		}
		for i := 0; i < actNumChngs; i++ {
			chng := chngs[i]
			// t.Log(string(chng.SerialisedForm))
			if chng.NumberOfTrxns != 1 {
				t.Errorf("Expected only one transaction in this change but found %d transactions", chng.NumberOfTrxns)
			}
			trxnRec := chng.Trxns[0]
			if trxnRec.Type != serverpb.TrxnRecord_Put {
				t.Errorf("Expected transaction type to be Put but found %s", trxnRec.Type.String())
			}
			expKey, expVal := fmt.Sprintf("%s_%d", keyPrefix, i+1), fmt.Sprintf("%s_%d", valPrefix, i+1)
			actKey, actVal := string(trxnRec.Key), string(trxnRec.Value)
			if expKey != actKey {
				t.Errorf("Key mismatch. Expected: %s, Actual: %s", expKey, actKey)
			}
			if expVal != actVal {
				t.Errorf("Value mismatch. Expected: %s, Actual: %s", expVal, actVal)
			}
		}
	}
}

func TestSaveChanges(t *testing.T) {
	numTrxns := 3
	putKeyPrefix, putValPrefix := "ccKey", "ccVal"
	putKeys(t, numTrxns, putKeyPrefix, putValPrefix)
	chngNum, _ := changePropagator.GetLatestCommittedChangeNumber()
	chngNum++ // due to possible previous transaction
	wbPutKeyPrefix, wbPutValPrefix := "ddKey", "ddVal"
	chngs := make([]*serverpb.ChangeRecord, numTrxns)
	for i := 0; i < numTrxns; i++ {
		wb := gorocksdb.NewWriteBatch()
		defer wb.Destroy()
		ks, vs := fmt.Sprintf("%s_%d", wbPutKeyPrefix, i+1), fmt.Sprintf("%s_%d", wbPutValPrefix, i+1)
		wb.Put([]byte(ks), []byte(vs))
		delKs := fmt.Sprintf("%s_%d", putKeyPrefix, i+1)
		wb.Delete([]byte(delKs))
		chngs[i] = toChangeRecord(wb, chngNum)
		chngNum++
	}
	expChngNum := chngNum - 1

	if actChngNum, err := changeApplier.SaveChanges(chngs); err != nil {
		t.Fatal(err)
	} else {
		if expChngNum != actChngNum {
			t.Errorf("Change numbers mismatch. Expected: %d, Actual: %d", expChngNum, actChngNum)
		}
		getKeys(t, numTrxns, wbPutKeyPrefix, wbPutValPrefix)
		noKeys(t, numTrxns, putKeyPrefix)
	}
}

// Following test can be removed once DKV supports bulk writes
func TestGetUpdatesFromSeqNumForBatches(t *testing.T) {
	rdb := store.(*rocksDB)
	beforeSeq := rdb.db.GetLatestSequenceNumber()

	expNumBatchTrxns := 3
	numTrxnsPerBatch := 2
	expNumTrxns := expNumBatchTrxns * numTrxnsPerBatch
	for i := 1; i <= expNumBatchTrxns; i++ {
		k, v := fmt.Sprintf("bKey_%d", i), fmt.Sprintf("bVal_%d", i)
		wb := gorocksdb.NewWriteBatch()
		wb.Put([]byte(k), []byte(v))
		wb.Delete([]byte(k))
		wo := gorocksdb.NewDefaultWriteOptions()
		wo.SetSync(true)
		if err := rdb.db.Write(wo, wb); err != nil {
			t.Fatal(err)
		}
		wb.Destroy()
		wo.Destroy()
	}

	afterSeq := rdb.db.GetLatestSequenceNumber()
	numTrxns := int(afterSeq - beforeSeq)
	if numTrxns != expNumTrxns {
		t.Errorf("Incorrect number of transactions reported. Expected: %d, Actual: %d", expNumTrxns, numTrxns)
	}

	startSeq := 1 + beforeSeq // This is done to remove previous transaction if any
	if trxnIter, err := rdb.db.GetUpdatesSince(startSeq); err != nil {
		t.Fatal(err)
	} else {
		defer trxnIter.Destroy()
		for trxnIter.Valid() {
			wb, _ := trxnIter.GetBatch()
			numTrxnsPerWb := wb.Count()
			if numTrxnsPerWb != numTrxnsPerBatch {
				t.Errorf("Incorrect number of transactions per batch. Expected: %d, Actual: %d", numTrxnsPerBatch, numTrxnsPerWb)
			}
			wbIter := wb.NewIterator()
			for wbIter.Next() {
				wbr := wbIter.Record()
				// t.Logf("Type: %v, Key: %s, Val: %s", wbr.Type, wbr.Key, wbr.Value)
				switch wbr.Type {
				case 1: // Put
					if !strings.HasPrefix(string(wbr.Key), "bKey_") {
						t.Errorf("Invalid key for PUT record. Value: %s", wbr.Key)
					}
					if !strings.HasPrefix(string(wbr.Value), "bVal_") {
						t.Errorf("Invalid value inside write batch record for key: %s. Value: %s", wbr.Key, wbr.Value)
					}
				case 0: // Delete
					if !strings.HasPrefix(string(wbr.Key), "bKey_") {
						t.Errorf("Invalid key for DELETE record. Value: %s", wbr.Key)
					}
				default:
					t.Errorf("Invalid type: %v", wbr.Type)
				}
			}
			wb.Destroy()
			trxnIter.Next()
		}
	}
}

func TestMultiGet(t *testing.T) {
	numKeys := 10
	keys, vals := make([][]byte, numKeys), make([]string, numKeys)
	for i := 1; i <= numKeys; i++ {
		key, value := fmt.Sprintf("MK%d", i), fmt.Sprintf("MV%d", i)
		if err := store.Put([]byte(key), []byte(value)); err != nil {
			t.Fatalf("Unable to PUT. Key: %s, Value: %s, Error: %v", key, value, err)
		} else {
			keys[i-1] = []byte(key)
			vals[i-1] = value
		}
	}

	if results, err := store.Get(keys...); err != nil {
		t.Fatal(err)
	} else {
		for i, result := range results {
			if string(result) != vals[i] {
				t.Errorf("Multi Get value mismatch. Key: %s, Expected Value: %s, Actual Value: %s", keys[i], vals[i], result)
			}
		}
	}
}

func TestMissingGet(t *testing.T) {
	key, expectedValue := "MissingKey", ""
	if readResults, err := store.Get([]byte(key)); err != nil {
		t.Fatal(err)
	} else if string(readResults[0]) != "" {
		t.Errorf("GET mismatch. Key: %s, Expected Value: %s, Actual Value: %s", key, expectedValue, readResults[0])
	}
}

func BenchmarkPutNewKeys(b *testing.B) {
	for i := 0; i < b.N; i++ {
		key, value := fmt.Sprintf("BK%d", i), fmt.Sprintf("BV%d", i)
		if err := store.Put([]byte(key), []byte(value)); err != nil {
			b.Fatalf("Unable to PUT. Key: %s, Value: %s, Error: %v", key, value, err)
		}
	}
}

func BenchmarkPutExistingKey(b *testing.B) {
	key := "BKey"
	if err := store.Put([]byte(key), []byte("BVal")); err != nil {
		b.Fatalf("Unable to PUT. Key: %s. Error: %v", key, err)
	}
	for i := 0; i < b.N; i++ {
		value := fmt.Sprintf("BVal%d", i)
		if err := store.Put([]byte(key), []byte(value)); err != nil {
			b.Fatalf("Unable to PUT. Key: %s, Value: %s, Error: %v", key, value, err)
		}
	}
}

func BenchmarkGetKey(b *testing.B) {
	key, val := "BGetKey", "BGetVal"
	if err := store.Put([]byte(key), []byte(val)); err != nil {
		b.Fatalf("Unable to PUT. Key: %s. Error: %v", key, err)
	}
	for i := 0; i < b.N; i++ {
		if readResults, err := store.Get([]byte(key)); err != nil {
			b.Fatalf("Unable to GET. Key: %s, Error: %v", key, err)
		} else if string(readResults[0]) != val {
			b.Errorf("GET mismatch. Key: %s, Expected Value: %s, Actual Value: %s", key, val, readResults[0])
		}
	}
}

func BenchmarkGetMissingKey(b *testing.B) {
	key := "BMissingKey"
	for i := 0; i < b.N; i++ {
		if _, err := store.Get([]byte(key)); err != nil {
			b.Fatalf("Unable to GET. Key: %s, Error: %v", key, err)
		}
	}
}

func noKeys(t *testing.T, numKeys int, keyPrefix string) {
	for i := 1; i <= numKeys; i++ {
		key := fmt.Sprintf("%s_%d", keyPrefix, i)
		if readResults, err := store.Get([]byte(key)); err != nil {
			t.Fatalf("Unable to GET. Key: %s, Error: %v", key, err)
		} else if string(readResults[0]) != "" {
			t.Errorf("Expected missing for key: %s. But found it with value: %s", key, readResults[0])
		}
	}
}

func getKeys(t *testing.T, numKeys int, keyPrefix, valPrefix string) {
	for i := 1; i <= numKeys; i++ {
		key, expectedValue := fmt.Sprintf("%s_%d", keyPrefix, i), fmt.Sprintf("%s_%d", valPrefix, i)
		if readResults, err := store.Get([]byte(key)); err != nil {
			t.Fatalf("Unable to GET. Key: %s, Error: %v", key, err)
		} else if string(readResults[0]) != expectedValue {
			t.Errorf("GET mismatch. Key: %s, Expected Value: %s, Actual Value: %s", key, expectedValue, readResults[0])
		}
	}
}

func putKeys(t *testing.T, numKeys int, keyPrefix, valPrefix string) {
	for i := 1; i <= numKeys; i++ {
		k, v := fmt.Sprintf("%s_%d", keyPrefix, i), fmt.Sprintf("%s_%d", valPrefix, i)
		if err := store.Put([]byte(k), []byte(v)); err != nil {
			t.Fatal(err)
		} else {
			if readResults, err := store.Get([]byte(k)); err != nil {
				t.Fatal(err)
			} else if string(readResults[0]) != string(v) {
				t.Errorf("GET mismatch. Key: %s, Expected Value: %s, Actual Value: %s", k, v, readResults[0])
			}
		}
	}
}

func openRocksDB() (*rocksDB, error) {
	if err := exec.Command("rm", "-rf", dbFolder).Run(); err != nil {
		return nil, err
	}
	opts := newDefaultOptions().DBFolder(dbFolder).CreateDBFolderIfMissing(createDBFolderIfMissing).CacheSize(cacheSize)
	return openStore(opts)
}
