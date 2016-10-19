package main_test

import (
	"errors"
	"io"
	"net/http"
	"testing"

	. "github.com/abustany/pfd"
)

func TestFilenameFromHeader(t *testing.T) {
	testData := []struct {
		Name   string
		Header string
		Valid  bool
		Result string
	}{
		{
			"Empty header",
			"",
			true,
			"",
		},
		{
			"Invalid data",
			"attachment; filename=\"fname.ext",
			false,
			"",
		},
		{
			"Disposition with filename",
			"attachment; filename=\"fname.ext\"",
			true,
			"fname.ext",
		},
		{
			"Disposition with absolute filename",
			"attachment; filename=\"/etc/fname.ext\"",
			true,
			"fname.ext",
		},
	}

	for _, test := range testData {
		h := http.Header{}
		h.Set("Content-Disposition", test.Header)

		filename, err := FilenameFromHeaders(h)

		if !test.Valid && err == nil {
			t.Errorf("Expected an error for case %s", test.Name)
		}

		if test.Valid && err != nil {
			t.Errorf("Unexpected error for test case %s: %s", test.Name, err)
		}

		if test.Result != filename {
			t.Errorf("Incorrect result for test case %s: expected '%s', got '%s'", test.Name, test.Result, filename)
		}
	}
}

func TestComputeChunkSize(t *testing.T) {
	testData := []struct {
		Name             string
		ContentLength    uint64
		RequestedWorkers uint
		NWorkers         uint
	}{
		{
			"10MB with default number of workers",
			10 * 1024 * 1024,
			0,
			DefaultNWorkers,
		},
		{
			"10MB with 3 workers",
			10 * 1024 * 1024,
			3,
			3,
		},
		{
			"10MB with too many workers",
			10 * 1024 * 1024,
			200,
			10,
		},
		{
			"Odd size with too many workers",
			10123456,
			200,
			10,
		},
		{
			"Tiny file with default number of workers",
			10,
			0,
			1,
		},
		{
			"Huge file with more than MaxNWorkers",
			2 * MaxNWorkers * MinimumChunkSize,
			MaxNWorkers + 1,
			MaxNWorkers,
		},
		{
			"Empty file",
			0,
			0,
			1,
		},
	}

	for _, test := range testData {
		nWorkers := ComputeNWorkers(test.ContentLength, test.RequestedWorkers)

		if nWorkers != test.NWorkers {
			t.Errorf("Incorrect result for test case %s: expected %d workers, got %d", test.Name, test.NWorkers, nWorkers)
		}
	}
}

type testReaderStep struct {
	data []byte
	err  error
}

type testReader struct {
	index uint
	steps []testReaderStep
}

func (r *testReader) Close() error {
	return nil
}

func (r *testReader) Read(buf []byte) (int, error) {
	if r.index >= uint(len(r.steps)) {
		panic("Invalid index")
	}

	data := r.steps[r.index].data

	if len(data) > len(buf) {
		data = data[0:len(buf)]
	}

	copy(buf, data)
	err := r.steps[r.index].err

	r.index++

	return len(data), err
}

func getNextDownloadedChunk(t *testing.T, resultChan <-chan DownloadedChunk) DownloadedChunk {
	select {
	case res := <-resultChan:
		return res
	default:
		t.Fatalf("No response from DownloadedChunk")
		return DownloadedChunk{}
	}
}

func TestDownloadChunkInitialError(t *testing.T) {
	fetcher := func(chunk Chunk) (io.ReadCloser, error) {
		return nil, errors.New("Nope")
	}

	ch := make(chan DownloadedChunk, 1)
	DownloadChunk(fetcher, Chunk{Offset: 0, Length: 10}, ch)

	res := getNextDownloadedChunk(t, ch)

	if res.Err == nil {
		t.Errorf("Error not set on result")
	}

	if res.Data != nil {
		t.Errorf("Data not nil on result")
	}
}

func TestDownloadChunkReadError(t *testing.T) {
	fetcher := func(chunk Chunk) (io.ReadCloser, error) {
		return &testReader{
			steps: []testReaderStep{
				{
					err: errors.New("Nope"),
				},
			},
		}, nil
	}

	ch := make(chan DownloadedChunk, 1)
	DownloadChunk(fetcher, Chunk{Offset: 0, Length: 10}, ch)

	res := getNextDownloadedChunk(t, ch)

	if res.Err == nil {
		t.Errorf("Error not set on result")
	}

	if len(res.Data) != 0 {
		t.Errorf("Data not nil on result")
	}
}

func TestDownloadChunkOneResult(t *testing.T) {
	testData := []byte("hello")

	fetcher := func(chunk Chunk) (io.ReadCloser, error) {
		return &testReader{
			steps: []testReaderStep{
				{
					data: testData,
				},
				{
					err: io.EOF,
				},
			},
		}, nil
	}

	ch := make(chan DownloadedChunk, 1)
	DownloadChunk(fetcher, Chunk{Offset: 0, Length: 10}, ch)

	res := getNextDownloadedChunk(t, ch)

	if res.Err != io.EOF {
		t.Errorf("Should get io.EOF on first chunk")
	}

	if res.Offset != 0 {
		t.Errorf("Incorrect offset on first chunk, expected 0, got %d", res.Offset)
	}

	if res.Data == nil {
		t.Errorf("Data in first chunk should not be nil")
	} else if string(res.Data) != string(testData) {
		t.Errorf("Incorrect data in first chunk, expected '%s', got '%s'", string(testData), string(res.Data))
	}
}

func TestDownloadChunkManyResults(t *testing.T) {
	testData := make([]byte, 2*MinimumChunkSize)

	for i := range testData {
		testData[i] = 'a'
	}

	fetcher := func(chunk Chunk) (io.ReadCloser, error) {
		return &testReader{
			steps: []testReaderStep{
				{
					data: testData[0:MinimumChunkSize],
				},
				{
					data: testData[MinimumChunkSize:],
					err:  io.EOF,
				},
			},
		}, nil
	}

	ch := make(chan DownloadedChunk, 2)
	DownloadChunk(fetcher, Chunk{Offset: 0, Length: 10}, ch)

	res := getNextDownloadedChunk(t, ch)

	if res.Err != nil {
		t.Errorf("Should not get any error on first chunk")
	}

	if res.Offset != 0 {
		t.Errorf("Incorrect offset on first chunk, expected 0, got %d", res.Offset)
	}

	if len(res.Data) != MinimumChunkSize {
		t.Errorf("Incorrect data length on first chunk, expected %d, got %d", MinimumChunkSize, len(res.Data))
	}

	res = getNextDownloadedChunk(t, ch)

	if res.Err != io.EOF {
		t.Errorf("Should get io.EOF on second chunk")
	}

	if res.Offset != MinimumChunkSize {
		t.Errorf("Incorrect offset on second chunk, expected %d, got %d", MinimumChunkSize, res.Offset)
	}

	if len(res.Data) != MinimumChunkSize {
		t.Errorf("Incorrect data length on second chunk, expected %d, got %d", MinimumChunkSize, len(res.Data))
	}
}

func checkCommand(t *testing.T, commands <-chan Chunk, expectedOffset uint64, expectedLength uint64) {
	command, ok := <-commands

	if !ok {
		t.Errorf("Commands channel is closed")
	}

	if command.Offset != expectedOffset {
		t.Errorf("Incorrect command offset, expected 0, got %d", command.Offset)
	}

	if command.Length != expectedLength {
		t.Errorf("Incorrect command length, expected %d, got %d", expectedLength, command.Length)
	}
}

func checkCommandsClosed(t *testing.T, commands <-chan Chunk) {
	command, ok := <-commands

	if ok {
		t.Errorf("Commands channel should have been closed, got command instead: %v", command)
	}
}

func TestCommandDownloadErrorAtFirstChunk(t *testing.T) {
	length := uint64(512)
	nWorkers := uint(1)
	results := make(chan DownloadedChunk, 1)
	chunkWriter := func(data []byte, offset uint64) error {
		return nil
	}
	commands := make(chan Chunk, 1)
	results <- DownloadedChunk{Err: errors.New("Nope")}

	err := CommandDownload(length, nWorkers, results, chunkWriter, commands)

	if err == nil {
		t.Errorf("CommandDownload should return an error")
	}

	checkCommand(t, commands, 0, length)
	checkCommandsClosed(t, commands)
}

func TestCommandDownloadErrorAtSecondChunk(t *testing.T) {
	length := uint64(2 * MinimumChunkSize)
	nWorkers := uint(1)
	results := make(chan DownloadedChunk, 2)
	chunkWriter := func(data []byte, offset uint64) error {
		return nil
	}
	commands := make(chan Chunk, 2)
	results <- DownloadedChunk{Offset: 0, Data: make([]byte, MinimumChunkSize)}
	results <- DownloadedChunk{Err: errors.New("Nope")}

	err := CommandDownload(length, nWorkers, results, chunkWriter, commands)

	if err == nil {
		t.Errorf("CommandDownload should return an error")
	}

	checkCommand(t, commands, 0, 2*MinimumChunkSize)
	checkCommandsClosed(t, commands)
}

func TestCommandDownloadOneWorker(t *testing.T) {
	length := uint64(2 * MinimumChunkSize)
	nWorkers := uint(1)
	results := make(chan DownloadedChunk, 2)
	downloaded := 0
	chunkWriter := func(data []byte, offset uint64) error {
		downloaded += len(data)
		return nil
	}
	commands := make(chan Chunk, 2)
	chunkData := make([]byte, MinimumChunkSize)
	results <- DownloadedChunk{Offset: 0, Data: chunkData}
	results <- DownloadedChunk{Offset: MinimumChunkSize, Data: chunkData, Err: io.EOF}

	err := CommandDownload(length, nWorkers, results, chunkWriter, commands)

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	checkCommand(t, commands, 0, 2*MinimumChunkSize)
	checkCommandsClosed(t, commands)

	if downloaded != 2*MinimumChunkSize {
		t.Errorf("Size of all received chunks does not match file size: expected %d, got %d", length, downloaded)
	}
}

func TestCommandDownloadTwoWorkers(t *testing.T) {
	length := uint64(2 * MinimumChunkSize)
	nWorkers := uint(2)
	results := make(chan DownloadedChunk, 2)
	downloaded := 0
	chunkWriter := func(data []byte, offset uint64) error {
		downloaded += len(data)
		return nil
	}
	commands := make(chan Chunk, 2)
	chunkData := make([]byte, MinimumChunkSize)
	results <- DownloadedChunk{Offset: 0, Data: chunkData, Err: io.EOF}
	results <- DownloadedChunk{Offset: MinimumChunkSize, Data: chunkData, Err: io.EOF}

	err := CommandDownload(length, nWorkers, results, chunkWriter, commands)

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	checkCommand(t, commands, 0, MinimumChunkSize)
	checkCommand(t, commands, MinimumChunkSize, MinimumChunkSize)
	checkCommandsClosed(t, commands)

	if downloaded != 2*MinimumChunkSize {
		t.Errorf("Size of all received chunks does not match file size: expected %d, got %d", length, downloaded)
	}
}

func TestRenderProgressBar(t *testing.T) {
	testData := []struct {
		Name     string
		Progress uint
		Size     uint
		Result   string
	}{
		{
			"Short progress bar",
			33,
			4,
			" 33%",
		},
		{
			"Normal progress bar, 0%",
			0,
			20,
			"  0% [>            ]",
		},
		{
			"Normal progress bar, 50%",
			50,
			20,
			" 50% [=====>       ]",
		},
		{
			"Normal progress bar, 95%",
			95,
			20,
			" 95% [===========> ]",
		},
		{
			"Normal progress bar, 100%",
			100,
			20,
			"100% [============>]",
		},
	}

	for _, test := range testData {
		res := RenderProgressBar(test.Progress, test.Size)

		if res != test.Result {
			t.Errorf("Unexpected result for progress %d and size %d: expected '%s', got '%s'", test.Progress, test.Size, test.Result, res)
		}
	}
}
