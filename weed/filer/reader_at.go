package filer

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"sync"

	"github.com/chrislusf/seaweedfs/weed/util/log"
	"github.com/chrislusf/seaweedfs/weed/pb/filer_pb"
	"github.com/chrislusf/seaweedfs/weed/util"
	"github.com/chrislusf/seaweedfs/weed/util/chunk_cache"
	"github.com/chrislusf/seaweedfs/weed/wdclient"
	"github.com/golang/groupcache/singleflight"
)

type ChunkReadAt struct {
	masterClient *wdclient.MasterClient
	chunkViews   []*ChunkView
	lookupFileId LookupFileIdFunctionType
	readerLock   sync.Mutex
	fileSize     int64

	fetchGroup      singleflight.Group
	lastChunkFileId string
	lastChunkData   []byte
	chunkCache      chunk_cache.ChunkCache
}

// var _ = io.ReaderAt(&ChunkReadAt{})

type LookupFileIdFunctionType func(fileId string) (targetUrls []string, err error)

func LookupFn(filerClient filer_pb.FilerClient) LookupFileIdFunctionType {

	vidCache := make(map[string]*filer_pb.Locations)
	var vicCacheLock sync.RWMutex
	return func(fileId string) (targetUrls []string, err error) {
		vid := VolumeId(fileId)
		vicCacheLock.RLock()
		locations, found := vidCache[vid]
		vicCacheLock.RUnlock()

		if !found {
			util.Retry("lookup volume "+vid, func() error {
				err = filerClient.WithFilerClient(func(client filer_pb.SeaweedFilerClient) error {
					resp, err := client.LookupVolume(context.Background(), &filer_pb.LookupVolumeRequest{
						VolumeIds: []string{vid},
					})
					if err != nil {
						return err
					}

					locations = resp.LocationsMap[vid]
					if locations == nil || len(locations.Locations) == 0 {
						log.Infof("failed to locate %s", fileId)
						return fmt.Errorf("failed to locate %s", fileId)
					}
					vicCacheLock.Lock()
					vidCache[vid] = locations
					vicCacheLock.Unlock()

					return nil
				})
				return err
			})
		}

		if err != nil {
			return nil, err
		}

		for _, loc := range locations.Locations {
			volumeServerAddress := filerClient.AdjustedUrl(loc)
			targetUrl := fmt.Sprintf("http://%s/%s", volumeServerAddress, fileId)
			targetUrls = append(targetUrls, targetUrl)
		}

		for i := len(targetUrls) - 1; i > 0; i-- {
			j := rand.Intn(i + 1)
			targetUrls[i], targetUrls[j] = targetUrls[j], targetUrls[i]
		}

		return
	}
}

func NewChunkReaderAtFromClient(filerClient filer_pb.FilerClient, chunkViews []*ChunkView, chunkCache chunk_cache.ChunkCache, fileSize int64) *ChunkReadAt {

	return &ChunkReadAt{
		chunkViews:   chunkViews,
		lookupFileId: LookupFn(filerClient),
		chunkCache:   chunkCache,
		fileSize:     fileSize,
	}
}

func (c *ChunkReadAt) ReadAt(p []byte, offset int64) (n int, err error) {

	c.readerLock.Lock()
	defer c.readerLock.Unlock()

	log.Tracef("ReadAt [%d,%d) of total file size %d bytes %d chunk views", offset, offset+int64(len(p)), c.fileSize, len(c.chunkViews))
	return c.doReadAt(p[n:], offset+int64(n))
}

func (c *ChunkReadAt) doReadAt(p []byte, offset int64) (n int, err error) {

	var buffer []byte
	startOffset, remaining := offset, int64(len(p))
	var nextChunk *ChunkView
	for i, chunk := range c.chunkViews {
		if remaining <= 0 {
			break
		}
		if i+1 < len(c.chunkViews) {
			nextChunk = c.chunkViews[i+1]
		} else {
			nextChunk = nil
		}
		if startOffset < chunk.LogicOffset {
			gap := int(chunk.LogicOffset - startOffset)
			log.Tracef("zero [%d,%d)", startOffset, startOffset+int64(gap))
			n += int(min(int64(gap), remaining))
			startOffset, remaining = chunk.LogicOffset, remaining-int64(gap)
			if remaining <= 0 {
				break
			}
		}
		// fmt.Printf(">>> doReadAt [%d,%d), chunk[%d,%d)\n", offset, offset+int64(len(p)), chunk.LogicOffset, chunk.LogicOffset+int64(chunk.Size))
		chunkStart, chunkStop := max(chunk.LogicOffset, startOffset), min(chunk.LogicOffset+int64(chunk.Size), startOffset+remaining)
		if chunkStart >= chunkStop {
			continue
		}
		log.Tracef("read [%d,%d), %d/%d chunk %s [%d,%d)", chunkStart, chunkStop, i, len(c.chunkViews), chunk.FileId, chunk.LogicOffset-chunk.Offset, chunk.LogicOffset-chunk.Offset+int64(chunk.Size))
		buffer, err = c.readFromWholeChunkData(chunk, nextChunk)
		if err != nil {
			log.Errorf("fetching chunk %+v: %v\n", chunk, err)
			return
		}
		bufferOffset := chunkStart - chunk.LogicOffset + chunk.Offset
		copied := copy(p[startOffset-offset:chunkStop-chunkStart+startOffset-offset], buffer[bufferOffset:bufferOffset+chunkStop-chunkStart])
		n += copied
		startOffset, remaining = startOffset+int64(copied), remaining-int64(copied)
	}

	log.Tracef("doReadAt [%d,%d), n:%v, err:%v", offset, offset+int64(len(p)), n, err)

	if err == nil && remaining > 0 && c.fileSize > startOffset {
		delta := int(min(remaining, c.fileSize-startOffset))
		log.Tracef("zero2 [%d,%d) of file size %d bytes", startOffset, startOffset+int64(delta), c.fileSize)
		n += delta
	}

	if err == nil && offset+int64(len(p)) >= c.fileSize {
		err = io.EOF
	}
	// fmt.Printf("~~~ filled %d, err: %v\n\n", n, err)

	return

}

func (c *ChunkReadAt) readFromWholeChunkData(chunkView *ChunkView, nextChunkViews ...*ChunkView) (chunkData []byte, err error) {

	if c.lastChunkFileId == chunkView.FileId {
		return c.lastChunkData, nil
	}

	v, doErr := c.readOneWholeChunk(chunkView)

	if doErr != nil {
		return nil, doErr
	}

	chunkData = v.([]byte)

	c.lastChunkData = chunkData
	c.lastChunkFileId = chunkView.FileId

	for _, nextChunkView := range nextChunkViews {
		if c.chunkCache != nil && nextChunkView != nil {
			go c.readOneWholeChunk(nextChunkView)
		}
	}

	return
}

func (c *ChunkReadAt) readOneWholeChunk(chunkView *ChunkView) (interface{}, error) {

	var err error

	return c.fetchGroup.Do(chunkView.FileId, func() (interface{}, error) {

		log.Tracef("readFromWholeChunkData %s offset %d [%d,%d) size at least %d", chunkView.FileId, chunkView.Offset, chunkView.LogicOffset, chunkView.LogicOffset+int64(chunkView.Size), chunkView.ChunkSize)

		data := c.chunkCache.GetChunk(chunkView.FileId, chunkView.ChunkSize)
		if data != nil {
			log.Tracef("cache hit %s [%d,%d)", chunkView.FileId, chunkView.LogicOffset-chunkView.Offset, chunkView.LogicOffset-chunkView.Offset+int64(len(data)))
		} else {
			var err error
			data, err = c.doFetchFullChunkData(chunkView)
			if err != nil {
				return data, err
			}
			c.chunkCache.SetChunk(chunkView.FileId, data)
		}
		return data, err
	})
}

func (c *ChunkReadAt) doFetchFullChunkData(chunkView *ChunkView) ([]byte, error) {

	log.Tracef("+ doFetchFullChunkData %s", chunkView.FileId)

	data, err := fetchChunk(c.lookupFileId, chunkView.FileId, chunkView.CipherKey, chunkView.IsGzipped)

	log.Tracef("- doFetchFullChunkData %s", chunkView.FileId)

	return data, err

}
