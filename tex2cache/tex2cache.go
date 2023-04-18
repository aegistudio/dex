// Package tex2cache provides caching mechanism for
// caching and scheduling the process of caching.
//
// The cache is done by SHA256 hashing the key, and
// append zero bytes to the end when there's collision.
package tex2cache

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/hashicorp/golang-lru"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

// cacheLoadRequest is the request to load content
// from the cache, based on the result, it will load
// the content as the result, or perform a new generate.
type cacheLoadRequest struct {
	key    []byte
	value  []byte
	ok     bool
	doneCh chan struct{}
}

// cacheStoreRequest is the request to store the content
// that has been generated.
type cacheStoreRequest struct {
	key   []byte
	value []byte
	ok    bool
}

// Cache is the interface object to accessing cache.
type Cache struct {
	ctx      context.Context
	lruCache *lru.Cache
	loadCh   chan *cacheLoadRequest
	storeCh  chan *cacheStoreRequest
}

// encodeKey encodes the key so that it could be indexed.
func encodeKey(key []byte) string {
	return base64.RawStdEncoding.EncodeToString(key)
}

// Load access the cache and retrieve the content.
func (cache *Cache) Load(
	key []byte, generate func() ([]byte, error),
) ([]byte, error) {
	// If the content has been generated, just
	// load the content into the cache.
	if result, ok := cache.lruCache.Get(encodeKey(key)); ok {
		return result.([]byte), nil
	}

	// Otherwise ask the cache to load the content
	// into the cache, and the result will be stored
	// in the result.
	loadReq := &cacheLoadRequest{
		key:    key,
		doneCh: make(chan struct{}),
	}
	select {
	case <-cache.ctx.Done():
		return nil, cache.ctx.Err()
	case cache.loadCh <- loadReq:
	}
	select {
	case <-cache.ctx.Done():
		return nil, cache.ctx.Err()
	case <-loadReq.doneCh:
	}
	if loadReq.ok {
		return loadReq.value, nil
	}

	// We are assigned to generate the content, and
	// after the generation we ask the cache to store
	// our content.
	done := false
	var value []byte
	defer func() {
		storeReq := &cacheStoreRequest{
			key:   key,
			value: value,
			ok:    done,
		}
		select {
		case <-cache.ctx.Done():
		case cache.storeCh <- storeReq:
		}
	}()
	var err error
	if value, err = generate(); err != nil {
		return nil, err
	}
	done = true
	return value, nil
}

func evaluateCachePath(base string, sum []byte) string {
	return filepath.Join(
		base,
		hex.EncodeToString(sum[0:1]),
		hex.EncodeToString(sum[1:2]),
		hex.EncodeToString(sum[2:3]),
		hex.EncodeToString(sum[3:4]),
		hex.EncodeToString(sum[4:5]),
		hex.EncodeToString(sum[5:])+".tex2cache",
	)
}

func readCacheData(data []byte) (key, value []byte, rerr error) {
	// Deflate and decode the content to see if it is what
	// we are waiting for.
	data, err := ioutil.ReadAll(flate.NewReader(bytes.NewBuffer(data)))
	if err != nil {
		return nil, nil, err
	}

	// Load and verify the content here.
	//
	// The file format is extremely simple and there's
	// no integrity verification. But this is okay to be
	// used in source file like scenario.
	keySize, keyLen := binary.Uvarint(data)
	if keyLen <= 0 {
		return nil, nil, errors.New("no key size")
	}
	data = data[keyLen:]
	if len(data) < int(keySize) {
		return nil, nil, errors.New("malformed key size")
	}
	return data[:int(keySize)], data[int(keySize):], nil
}

func loadCacheFile(readDir string, key []byte) ([]byte, error) {
	hashing := sha256.New()
	if _, err := hashing.Write(key); err != nil {
		return nil, errors.Wrap(err, "write key data")
	}
	for {
		// Open the file at specified hash value.
		sum := hashing.Sum(nil)
		name := evaluateCachePath(readDir, sum)
		data, err := ioutil.ReadFile(name)
		if err != nil {
			return nil, errors.Wrapf(err, "read cache file %q", name)
		}

		// Judge whether the data is what we want.
		keyFile, value, err := readCacheData(data)
		if err != nil {
			// Ignore the corrupted file and continue on.
			continue
		}
		if bytes.Equal(keyFile, key) {
			return value, nil
		}

		// Move onto next file when there's collision in content.
		if _, err := hashing.Write([]byte{0x0}); err != nil {
			return nil, err
		}
	}
}

func marshalCacheData(key, value []byte) ([]byte, error) {
	var buf bytes.Buffer
	writer, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		return nil, errors.Wrap(err, "create flate writer")
	}
	var keySize [binary.MaxVarintLen64]byte
	keyLen := binary.PutUvarint(keySize[:], uint64(len(key)))
	var data []byte
	data = append(data, keySize[:keyLen]...)
	data = append(data, key...)
	data = append(data, value...)
	if _, err := writer.Write(data); err != nil {
		return nil, errors.Wrap(err, "write flate writer")
	}
	if err := writer.Close(); err != nil {
		return nil, errors.Wrap(err, "close flate writer")
	}
	return buf.Bytes(), nil
}

func writeCacheFile(writeDir string, key, value []byte) error {
	hashing := sha256.New()
	if _, err := hashing.Write(key); err != nil {
		return errors.Wrap(err, "write key data")
	}
	var name string
	for {
		// Open the file at specified hash value.
		sum := hashing.Sum(nil)
		name = evaluateCachePath(writeDir, sum)
		data, err := ioutil.ReadFile(name)
		if errors.Is(err, os.ErrNotExist) {
			break
		} else if err != nil {
			return errors.Wrapf(err, "read cache file %q", name)
		}

		// Read and judge whether we should write content here.
		if data != nil {
			keyFile, valueFile, err := readCacheData(data)
			if err != nil {
				// We will overwrite the corrupted one.
				break
			}
			if bytes.Equal(keyFile, key) {
				if bytes.Equal(value, valueFile) {
					return nil
				}
				break
			}
		}

		// Move onto next file when there's collision in content.
		if _, err := hashing.Write([]byte{0x0}); err != nil {
			return err
		}
	}

	// Create and write out the content of the cache.
	data, err := marshalCacheData(key, value)
	if err != nil {
		return errors.Wrap(err, "marshal cache data")
	}
	dir := filepath.Dir(name)
	if err := os.MkdirAll(
		dir, os.FileMode(0755)); err != nil {
		return errors.Wrapf(err, "make cache dir %q", dir)
	}
	if err := ioutil.WriteFile(
		name, data, os.FileMode(0644)); err != nil {
		return errors.Wrapf(err, "write cache file %q", name)
	}
	return nil
}

// runScheduler executes the main scheduler of the cache.
func (cache *Cache) runScheduler(readDir, writeDir string) error {
	readDir = filepath.Clean(readDir)
	writeDir = filepath.Clean(writeDir)
	requests := make(map[string][]*cacheLoadRequest)
	for {
		select {
		case <-cache.ctx.Done():
			return nil
		case loadReq := <-cache.loadCh:
			if err := func() error {
				key := loadReq.key
				keyEncoded := encodeKey(key)

				// It might have been loaded, so we will ask
				// it to load the cache first.
				if result, ok := cache.lruCache.Get(keyEncoded); ok {
					loadReq.value = result.([]byte)
					loadReq.ok = true
					close(loadReq.doneCh)
					return nil
				}

				// Attempt to load the file that has already been
				// on the local disk.
				value, err := loadCacheFile(readDir, key)
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				if err == nil {
					_ = cache.lruCache.Add(keyEncoded, value)
					loadReq.value = value
					loadReq.ok = true
					close(loadReq.doneCh)
					if err := writeCacheFile(
						writeDir, key, value); err != nil {
						return err
					}
					return nil
				}

				loadReqs := append(requests[keyEncoded], loadReq)
				if len(loadReqs) == 1 {
					// We would like the new queue item to work.
					close(loadReq.doneCh)
				}
				requests[keyEncoded] = loadReqs
				return nil
			}(); err != nil {
				return err
			}
		case storeReq := <-cache.storeCh:
			if err := func() error {
				key, value := storeReq.key, storeReq.value
				keyEncoded := encodeKey(key)
				loadReqs := requests[keyEncoded][1:]
				if storeReq.ok {
					// Notify all remaining items in the queue
					// that content is ready and good to go.
					if err := writeCacheFile(
						writeDir, key, value); err != nil {
						return err
					}
					_ = cache.lruCache.Add(keyEncoded, value)
					for _, loadReq := range loadReqs {
						loadReq.ok = true
						loadReq.value = value
						close(loadReq.doneCh)
					}
					delete(requests, keyEncoded)
				} else if len(loadReqs) > 0 {
					// Pull up the next one to process the file.
					close(loadReqs[0].doneCh)
					requests[keyEncoded] = loadReqs
				} else {
					delete(requests, keyEncoded)
				}
				return nil
			}(); err != nil {
				return err
			}
		}
	}
}

// New creates the cache object and so that they could
// be used for application here.
func New(
	ctx context.Context, group *errgroup.Group,
	readDir, writeDir string, cacheSize int,
) (*Cache, error) {
	lruCache, err := lru.New(cacheSize)
	if err != nil {
		return nil, err
	}
	result := &Cache{
		ctx:      ctx,
		lruCache: lruCache,
		loadCh:   make(chan *cacheLoadRequest),
		storeCh:  make(chan *cacheStoreRequest),
	}
	group.Go(func() error {
		return result.runScheduler(readDir, writeDir)
	})
	return result, nil
}
