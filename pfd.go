package main

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
)

type DownloadInfo struct {
	Url          string
	NWorkers     uint
	HTTPUser     string
	HTTPPassword string
}

func usage() {
	fmt.Fprintf(os.Stderr, "PFD - Parallel File Downloader\n\n")
	fmt.Fprintf(os.Stderr, "Usage: %s [OPTIONS] URL\n", os.Args[0])
	flag.PrintDefaults()
}

func FilenameFromHeaders(headers http.Header) (string, error) {
	disposition := headers.Get("Content-Disposition")

	if disposition == "" {
		return "", nil
	}

	_, params, err := mime.ParseMediaType(disposition)

	if err != nil {
		return "", err
	}

	return path.Base(params["filename"]), nil
}

func LengthFromHeaders(headers http.Header) (length uint64, present bool, err error) {
	lengthStr := headers.Get("Content-Length")

	if lengthStr == "" {
		present = false
		return
	}

	present = true
	length, err = strconv.ParseUint(lengthStr, 10, 64)
	return
}

const MinimumChunkSize = 1024 * 1024 // bytes
const DefaultNWorkers = 6
const MaxNWorkers = 500 // More than 500 parallel connections makes no sense

func ComputeNWorkers(contentLength uint64, requestedWorkers uint) uint {
	if contentLength == 0 {
		return 1
	}

	if requestedWorkers == 0 {
		requestedWorkers = DefaultNWorkers
	}

	var maxNWorkers uint64

	if contentLength <= MinimumChunkSize {
		maxNWorkers = 1
	} else {
		maxNWorkers = 1 + (contentLength-1)/MinimumChunkSize
	}

	if maxNWorkers > MaxNWorkers {
		maxNWorkers = MaxNWorkers
	}

	nWorkers := requestedWorkers

	if nWorkers == 0 {
		nWorkers = 6
	}

	if nWorkers > uint(maxNWorkers) {
		nWorkers = uint(maxNWorkers)
		fmt.Fprintf(os.Stderr, "Reducing the number of workers to %d\n", nWorkers)
	}

	return nWorkers
}

type Chunk struct {
	Offset uint64
	Length uint64
}

func (c Chunk) String() string {
	return fmt.Sprintf("[%d-%d[", c.Offset, c.Offset+c.Length)
}

type DownloadedChunk struct {
	Offset uint64
	Data   []byte // Downloaded data (can be less than request if an error occured)
	Err    error  // Error, if any
}

type RangeFetcher func(chunk Chunk) (io.ReadCloser, error)

func DownloadChunk(fetcher RangeFetcher, chunk Chunk, resultChan chan<- DownloadedChunk) {
	res := DownloadedChunk{
		Offset: chunk.Offset,
	}

	data, err := fetcher(chunk)

	if err != nil {
		res.Err = err
		resultChan <- res
		return
	}

	defer data.Close()

	buf := make([]byte, MinimumChunkSize)
	cur := 0

	for {
		n, err := data.Read(buf[cur:])
		cur += n

		res.Err = err

		if cur == len(buf) || err != nil {
			log.Printf("Read %d", cur)
			res.Data = make([]byte, cur)
			copy(res.Data, buf[0:cur])
			resultChan <- res
			res.Offset += uint64(cur)
			cur = 0
		}

		if err != nil {
			return
		}
	}
}

func writeChunk(fd io.WriteSeeker, chunk DownloadedChunk) error {
	if len(chunk.Data) == 0 {
		return nil
	}

	if _, err := fd.Seek(int64(chunk.Offset), io.SeekStart); err != nil {
		return err
	}

	if _, err := fd.Write(chunk.Data); err != nil {
		return err
	}

	return nil
}

func getFileInfo(dlinfo DownloadInfo) (filename string, length uint64, err error) {
	req, err := http.NewRequest("HEAD", dlinfo.Url, nil)

	if err != nil {
		return
	}

	setupAuthHeaders(dlinfo, req)

	resp, err := http.DefaultClient.Do(req)

	if err != nil {
		return
	}

	resp.Body.Close()

	if resp.Proto == "HTTP/1.0" {
		err = errors.New("HTTP/1.0 does not allow range requests")
		return
	}

	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("Unexpected status code from server: %d", resp.StatusCode)
		return
	}

	if resp.Header.Get("Accept-Ranges") != "bytes" {
		err = errors.New("Server does not support range requests")
		return
	}

	filename, err = FilenameFromHeaders(resp.Header)

	if err != nil {
		return
	}

	if filename == "" {
		filename = path.Base(dlinfo.Url)
	}

	if filename == "" {
		err = errors.New("Could not determine filename")
		return
	}

	length, hasLength, err := LengthFromHeaders(resp.Header)

	if err != nil {
		return
	}

	if !hasLength {
		err = errors.New("No Content-Length provided by server\n")
		return
	}

	return
}

func setupAuthHeaders(dlinfo DownloadInfo, req *http.Request) {
	if dlinfo.HTTPUser == "" {
		return
	}

	authData := bytes.NewBuffer(nil)

	authData.WriteString("Basic ")

	enc := base64.NewEncoder(base64.RawStdEncoding, authData)
	io.WriteString(enc, dlinfo.HTTPUser)
	enc.Write([]byte{':'})
	io.WriteString(enc, dlinfo.HTTPPassword)
	enc.Close()

	req.Header.Set("Authorization", authData.String())
}

func makeHttpRangeFetcher(dlinfo DownloadInfo) RangeFetcher {
	return func(chunk Chunk) (io.ReadCloser, error) {
		req, err := http.NewRequest("GET", dlinfo.Url, nil)

		if err != nil {
			return nil, err
		}

		// We can't have compression, else ranges don't make sense
		req.Header.Set("Accept-Encoding", "identity")
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", chunk.Offset, chunk.Offset+chunk.Length-1))

		setupAuthHeaders(dlinfo, req)

		resp, err := http.DefaultClient.Do(req)

		if err != nil {
			return nil, err
		}

		if resp.StatusCode != http.StatusPartialContent {
			return nil, fmt.Errorf("Unexpected HTTP status: %d", resp.StatusCode)
		}

		contentEncoding := resp.Header.Get("Content-Encoding")

		if contentEncoding != "" && contentEncoding != "identity" {
			return nil, fmt.Errorf("Server uses unsupported Content-Encoding %s", contentEncoding)
		}

		return resp.Body, nil
	}
}

func CreateInitialChunks(length uint64, nWorkers uint) []Chunk {
	chunks := make([]Chunk, 0, nWorkers)
	chunkSize := length / uint64(nWorkers)

	for i := uint(0); i < nWorkers; i++ {
		start := uint64(i) * chunkSize
		end := start + chunkSize

		if end >= length || i == (nWorkers-1) {
			end = length
		}

		chunk := Chunk{
			Offset: start,
			Length: end - start,
		}

		chunks = append(chunks, chunk)
	}

	return chunks
}

type ChunkWriter func(data []byte, offset uint64) error

func CommandDownload(length uint64, nWorkers uint, results <-chan DownloadedChunk, chunkWriter ChunkWriter, commands chan<- Chunk) error {
	chunks := CreateInitialChunks(length, nWorkers)

	if len(chunks) == 0 {
		close(commands)
		return nil
	}

	remainingChunks := make(map[uint64]Chunk, len(chunks))

	for _, chunk := range chunks {
		remainingChunks[chunk.Offset] = chunk

		select {
		case commands <- chunk:
			log.Printf("Queued chunk %s for download", chunk)
		default:
			// All workers busy, we'll send the chunks later
		}
	}

	for res := range results {
		if res.Err != nil && res.Err != io.EOF {
			close(commands)
			return res.Err
		}

		chunk, ok := remainingChunks[res.Offset]

		if !ok {
			log.Fatalf("Unknown chunk offset %d", res.Offset)
		}

		if err := chunkWriter(res.Data, res.Offset); err != nil {
			close(commands)
			return err
		}

		delete(remainingChunks, res.Offset)

		log.Printf("Chunk length before: %d -- %d", chunk.Length, len(res.Data))
		chunk.Offset += uint64(len(res.Data))
		chunk.Length -= uint64(len(res.Data))

		log.Printf("Wrote chunk [%d-%d[", res.Offset, res.Offset+uint64(len(res.Data)))
		log.Printf("Chunk size remaining: %d", chunk.Length)
		log.Printf("%d chunks remaining", len(remainingChunks))

		if chunk.Length > 0 {
			// The chunk hasn't been entirely downloaded, add the remaining part
			// back in the queue

			if _, ok := remainingChunks[chunk.Offset]; ok {
				log.Fatalf("Duplicate chunk at offset %d", chunk.Offset)
			}

			remainingChunks[chunk.Offset] = chunk
		}

		if len(remainingChunks) == 0 {
			close(commands)
			return nil
		}
	}

	panic("unreachable")
}

func RenderProgressBar(progress, size uint) string {
	if progress > 100 {
		progress = 100
	}

	// Progress bar structure:
	// 000% [====>  ]

	buf := bytes.NewBuffer(nil)
	fmt.Fprintf(buf, "%3d%%", progress)

	if size < 16 {
		return buf.String()
	}

	buf.WriteString(" [")

	fullProgressSize := size - 7
	scaledProgress := uint(float32(progress) / 100.0 * float32(fullProgressSize))

	log.Printf("Space: %d", fullProgressSize)
	log.Printf("Progress: %d", progress)
	log.Printf("Scaled progress: %d", scaledProgress)

	if scaledProgress > 0 {
		for i := uint(0); i < scaledProgress-1; i++ {
			buf.WriteByte('=')
		}
	}

	buf.WriteByte('>')

	for uint(buf.Len()) < size-1 {
		buf.WriteByte(' ')
	}

	buf.WriteString("]")

	return buf.String()
}

func printProgressBar(progress uint) {
	twidth, err := GetTerminalWidth()

	if err != nil {
		twidth = 80
	}

	os.Stderr.Write([]byte{'\r'})
	io.WriteString(os.Stderr, RenderProgressBar(progress, twidth))
}

func download(dlinfo DownloadInfo) error {
	filename, length, err := getFileInfo(dlinfo)

	if err != nil {
		return err
	}

	nWorkers := ComputeNWorkers(length, dlinfo.NWorkers)

	fd, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0666)

	if err != nil {
		return err
	}

	defer fd.Close()

	chunkChan := make(chan Chunk, nWorkers)
	resultChan := make(chan DownloadedChunk, nWorkers)
	fetcher := makeHttpRangeFetcher(dlinfo)
	written := uint64(0)
	chunkWriter := func(data []byte, offset uint64) error {
		if _, err := fd.Seek(int64(offset), os.SEEK_SET); err != nil {
			return err
		}

		if _, err := fd.Write(data); err != nil {
			return err
		}

		written += uint64(len(data))
		progress := uint(100.0 * float64(written) / float64(length))

		printProgressBar(progress)

		if progress == 100 {
			os.Stderr.Write([]byte{'\n'})
		}

		return nil
	}

	// Start workers
	for i := uint(0); i < nWorkers; i++ {
		go func() {
			for chunk := range chunkChan {
				DownloadChunk(fetcher, chunk, resultChan)
			}
		}()
	}

	return CommandDownload(length, nWorkers, resultChan, chunkWriter, chunkChan)
}

type nullWriter struct{}

func (w *nullWriter) Write(data []byte) (int, error) {
	return len(data), nil
}

func decodeUserPass(data string) (user string, password string, err error) {
	tokens := strings.SplitN(data, ":", 2)

	if len(tokens) != 2 {
		err = errors.New("Missing password")
		return
	}

	user = tokens[0]
	password = tokens[1]
	return
}

func main() {
	requestedWorkers := flag.Uint("n", DefaultNWorkers, "Number of parallel connections")
	insecure := flag.Bool("insecure", false, "Allow insecure HTTPS connections")
	auth := flag.String("auth", "", "HTTP user and password in the form user:password")
	debug := flag.Bool("debug", false, "Show debug messages")
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() == 0 {
		fmt.Fprintf(os.Stderr, "Missing URL on command line\n\n")
		usage()
		os.Exit(1)
	}

	if flag.NArg() > 1 {
		fmt.Fprintf(os.Stderr, "Too many URLs specified on the command line\n\n")
		usage()
		os.Exit(1)
	}

	dlurl := flag.Args()[0]

	if *insecure {
		http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}

	if !*debug {
		log.SetOutput(&nullWriter{})
	}

	dlinfo := DownloadInfo{
		Url:      dlurl,
		NWorkers: *requestedWorkers,
	}

	if *auth != "" {
		user, password, err := decodeUserPass(*auth)

		if err != nil {
			fmt.Fprintf(os.Stderr, "Invalid authentication data %s: %s", *auth, err)
			os.Exit(1)
		}

		dlinfo.HTTPUser = user
		dlinfo.HTTPPassword = password
	}

	err := download(dlinfo)

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error while downloading %s: %s\n", dlurl, err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Successfully downloaded %s\n", dlurl)
}
