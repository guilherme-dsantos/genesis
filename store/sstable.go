package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"

	"github.com/tferdous17/genesis/utils"
)

const (
	DataFileExtension  string = ".data"
	IndexFileExtension string = ".index"
	BloomFileExtension string = ".bloom"

	SparseIndexSampleSize int = 1000
)

var sstTableCounter uint32

type SSTable struct {
	dataFile    *os.File
	indexFile   *os.File
	bloomFilter *BloomFilter
	sstCounter  uint32
	minKey      string
	maxKey      string
	sizeInBytes uint32
	sparseKeys  []sparseIndex
}

// InitSSTableOnDisk directory to store sstable, (sorted) entries to store in said table
func InitSSTableOnDisk(directory string, entries *[]Record) *SSTable {
	atomic.AddUint32(&sstTableCounter, 1)
	table := &SSTable{
		sstCounter: sstTableCounter,
	}
	table.InitTableFiles(directory)
	writeEntriesToSST(entries, table)

	return table
}

func (sst *SSTable) InitTableFiles(directory string) error {
	// Create "storage" folder with read-write-execute for owner & group, read-only for others
	if err := os.MkdirAll("../storage", 0755); err != nil {
		return err
	}

	// create data and index files
	dataFile, err := os.Create(getNextSstFilename(directory, sst.sstCounter) + DataFileExtension)

	if err != nil {
		return fmt.Errorf("failed to create data file: %w", err)
	}

	indexFile, err := os.Create(getNextSstFilename(directory, sst.sstCounter) + IndexFileExtension)

	if err != nil {
		dataFile.Close() // Clean up previously created files
		return fmt.Errorf("failed to create index file: %w", err)
	}

	bloomFile, err := os.Create(getNextSstFilename(directory, sst.sstCounter) + BloomFileExtension)

	if err != nil {
		dataFile.Close() // Clean up previously created files
		indexFile.Close()
		return fmt.Errorf("failed to create bloom filter file: %w", err)
	}

	sst.dataFile, sst.indexFile = dataFile, indexFile
	sst.bloomFilter = NewBloomFilter(bloomFile)

	return nil
}

func getNextSstFilename(directory string, sstCounter uint32) string {
	return fmt.Sprintf("../%s/sst_%d", directory, sstCounter)
}

type sparseIndex struct {
	keySize    uint32
	key        string
	byteOffset uint32 // where to start reading from
}

func writeEntriesToSST(sortedEntries *[]Record, table *SSTable) {
	buf := new(bytes.Buffer)
	var byteOffsetCounter uint32

	// Keep track of min, max for searching in the case our desired key is outside these bounds
	table.minKey = (*sortedEntries)[0].Key
	table.maxKey = (*sortedEntries)[len(*sortedEntries)-1].Key

	// * every 1000th key will be put into the sparse index
	for i := range *sortedEntries {
		table.sizeInBytes += (*sortedEntries)[i].RecordSize
		if i%SparseIndexSampleSize == 0 {
			table.sparseKeys = append(table.sparseKeys, sparseIndex{
				keySize:    (*sortedEntries)[i].Header.KeySize,
				key:        (*sortedEntries)[i].Key,
				byteOffset: byteOffsetCounter,
			})
		}
		byteOffsetCounter += (*sortedEntries)[i].RecordSize
		(*sortedEntries)[i].EncodeKV(buf)
	}

	// after encoding all entries, dump into the SSTable
	if err := utils.WriteToFile(buf.Bytes(), table.dataFile); err != nil {
		fmt.Println("write to sst err:", err)
	}
	// * Set up sparse index
	utils.Logf("SPARSE KEYS: %v", table.sparseKeys)
	populateSparseIndexFile(&table.sparseKeys, table.indexFile)

	// * Set up + populate bloom filter
	table.bloomFilter.InitBloomFilterAttrs(uint32(len(*sortedEntries)))
	populateBloomFilter(sortedEntries, table.bloomFilter)
}

func populateSparseIndexFile(indices *[]sparseIndex, indexFile *os.File) {
	// encode and write to index file
	buf := new(bytes.Buffer)
	for i := range *indices {
		binary.Write(buf, binary.LittleEndian, (*indices)[i].keySize)
		buf.WriteString((*indices)[i].key)
		binary.Write(buf, binary.LittleEndian, (*indices)[i].byteOffset)
	}

	if err := utils.WriteToFile(buf.Bytes(), indexFile); err != nil {
		fmt.Println("write to indexfile err:", err)
	}
}

func populateBloomFilter(entries *[]Record, bloomFilter *BloomFilter) {
	for i := range *entries {
		bloomFilter.Add((*entries)[i].Key)
	}

	bfBytes := make([]byte, bloomFilter.bitSetSize)
	for i, b := range bloomFilter.bitSet {
		if b {
			bfBytes[i] = 1
		} else {
			bfBytes[i] = 0
		}
	}
	if err := utils.WriteToFile(bfBytes, bloomFilter.file); err != nil {
		fmt.Println("write to bloomfile err:", err)
	}
}

func (sst *SSTable) Get(key string) (string, error) {
	if key < sst.minKey || key > sst.maxKey {
		return "<!>", utils.ErrKeyNotWithinTable
	}

	if !sst.bloomFilter.MightContain(key) {
		utils.LogRED("BLOOM FILTER: %s is not a member of this table", key)
		return "", utils.ErrKeyNotWithinTable
	}

	// * Get sparse index and move to offset
	currOffset := sst.sparseKeys[sst.getCandidateByteOffsetIndex(key)].byteOffset
	if _, err := sst.dataFile.Seek(int64(currOffset), 0); err != nil {
		return "", err
	}
	// * start loop
	var keyFound = false
	var eofErr error

	for keyFound == false || eofErr == nil {
		// * set up entry for the header
		currEntry := make([]byte, 17)
		_, err := io.ReadFull(sst.dataFile, currEntry)
		if errors.Is(err, io.EOF) {
			eofErr = err
			return "", err
		}

		h := &Header{}
		h.DecodeHeader(currEntry)

		// * move the cursor so we can read the rest of the record
		currOffset += headerSize // can do this since headerSize is constant
		sst.dataFile.Seek(int64(currOffset), 0)
		// * set up []byte for the rest of the record
		currRecord := make([]byte, h.KeySize+h.ValueSize)
		if _, err := io.ReadFull(sst.dataFile, currRecord); err != nil {
			fmt.Println("READFULL ERR:", err)
			return "", err
		}
		// * append both []byte together in order to decode as a whole
		currEntry = append(currEntry, currRecord...) // full size of the record
		r := &Record{}
		r.DecodeKV(currEntry)
		//utils.Logf("LOOKING AT RECORD: %v", r)

		if r.Key == key {
			utils.LogGREEN("FOUND KEY %s -> VALUE %s\n", key, r.Value)
			keyFound = true
			return r.Value, nil
		} else if r.Key > key {
			// * return early
			// * this works b/c since our data is sorted, if the curr key is > target key,
			// * ..then the key is not in this table
			return "", utils.ErrKeyNotWithinTable
		} else {
			// * else, need to keep iterating & looking
			currOffset += r.Header.KeySize + r.Header.ValueSize
			sst.dataFile.Seek(int64(currOffset), 0)
		}

	}

	return "", utils.ErrKeyNotFound
}

func (sst *SSTable) getCandidateByteOffsetIndex(targetKey string) int {
	low := 0
	high := len(sst.sparseKeys) - 1

	for low <= high {
		mid := (low + high) / 2

		cmp := strings.Compare(targetKey, sst.sparseKeys[mid].key)
		if cmp > 0 { // targetKey > sparseKeys[mid]
			low = mid + 1
		} else if cmp < 0 { // targetKey < sparseKeys[mid]
			high = mid - 1
		} else { // equal
			return mid
		}
	}
	utils.LogCYAN("CANDIDATE BYTE OFFSET: %d AT INDEX %d", sst.sparseKeys[low-1].byteOffset, uint32(low-1))
	return low - 1
}
