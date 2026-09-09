package file

import (
	"encoding/binary"
	"io"
	"os"
	"strings"
)

// ReadMediaDuration reads the duration from a media file header.
// Supports MP4/MOV/M4V (ISO Base Media), WAV, and basic MP3.
// Returns duration in seconds, or 0 if it cannot be determined.
func ReadMediaDuration(path string) float64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	ext := strings.ToLower(path)
	switch {
	case strings.HasSuffix(ext, ".mp4"), strings.HasSuffix(ext, ".mov"), strings.HasSuffix(ext, ".m4v"):
		return readMP4Duration(f)
	case strings.HasSuffix(ext, ".wav"):
		return readWAVDuration(f)
	case strings.HasSuffix(ext, ".mp3"):
		return readMP3Duration(f)
	}
	return 0
}

// readMP4Duration reads the duration from an MP4 file's 'mvhd' box.
func readMP4Duration(r io.ReadSeeker) float64 {
	moovStart, moovEnd := findBox(r, "moov")
	if moovStart < 0 {
		return 0
	}
	if _, err := r.Seek(moovStart+8, io.SeekStart); err != nil {
		return 0
	}
	limited := io.LimitReader(r, moovEnd-moovStart-8)
	mvhdStart, _ := findBoxInReader(limited, "mvhd")
	if mvhdStart < 0 {
		return 0
	}
	absPos := moovStart + 8 + mvhdStart
	if _, err := r.Seek(absPos+8, io.SeekStart); err != nil {
		return 0
	}
	var version [1]byte
	if _, err := r.Read(version[:]); err != nil {
		return 0
	}
	if _, err := r.Seek(3, io.SeekCurrent); err != nil {
		return 0
	}
	if version[0] == 0 {
		if _, err := r.Seek(8, io.SeekCurrent); err != nil {
			return 0
		}
		var timescale, duration uint32
		if err := binary.Read(r, binary.BigEndian, &timescale); err != nil {
			return 0
		}
		if err := binary.Read(r, binary.BigEndian, &duration); err != nil {
			return 0
		}
		if timescale == 0 {
			return 0
		}
		return float64(duration) / float64(timescale)
	}
	if _, err := r.Seek(16, io.SeekCurrent); err != nil {
		return 0
	}
	var timescale uint32
	if err := binary.Read(r, binary.BigEndian, &timescale); err != nil {
		return 0
	}
	var duration uint64
	if err := binary.Read(r, binary.BigEndian, &duration); err != nil {
		return 0
	}
	if timescale == 0 {
		return 0
	}
	return float64(duration) / float64(timescale)
}

// readWAVDuration reads the duration from a WAV file header.
func readWAVDuration(r io.ReadSeeker) float64 {
	var riffID [4]byte
	if _, err := io.ReadFull(r, riffID[:]); err != nil {
		return 0
	}
	if string(riffID[:]) != "RIFF" {
		return 0
	}
	if _, err := r.Seek(4, io.SeekCurrent); err != nil {
		return 0
	}
	var waveID [4]byte
	if _, err := io.ReadFull(r, waveID[:]); err != nil {
		return 0
	}
	if string(waveID[:]) != "WAVE" {
		return 0
	}

	var sampleRate uint32
	var channels uint16
	var bitsPerSample uint16
	var dataSize uint32

	for {
		var chunkID [4]byte
		if _, err := io.ReadFull(r, chunkID[:]); err != nil {
			break
		}
		var chunkSize uint32
		if err := binary.Read(r, binary.LittleEndian, &chunkSize); err != nil {
			break
		}

		switch string(chunkID[:]) {
		case "fmt ":
			if chunkSize < 16 {
				return 0
			}
			if _, err := r.Seek(2, io.SeekCurrent); err != nil {
				return 0
			}
			if err := binary.Read(r, binary.LittleEndian, &channels); err != nil {
				return 0
			}
			if err := binary.Read(r, binary.LittleEndian, &sampleRate); err != nil {
				return 0
			}
			if _, err := r.Seek(6, io.SeekCurrent); err != nil {
				return 0
			}
			if err := binary.Read(r, binary.LittleEndian, &bitsPerSample); err != nil {
				return 0
			}
			remaining := int64(chunkSize) - 16
			if remaining > 0 {
				if _, err := r.Seek(remaining, io.SeekCurrent); err != nil {
					return 0
				}
			}
		case "data":
			dataSize = chunkSize
			if _, err := r.Seek(int64(chunkSize), io.SeekCurrent); err != nil {
				return 0
			}
		default:
			if _, err := r.Seek(int64(chunkSize), io.SeekCurrent); err != nil {
				return 0
			}
		}
	}

	if sampleRate == 0 || channels == 0 || bitsPerSample == 0 {
		return 0
	}

	bytesPerSecond := uint64(sampleRate) * uint64(channels) * uint64(bitsPerSample) / 8
	if bytesPerSecond == 0 {
		return 0
	}
	return float64(dataSize) / float64(bytesPerSecond)
}

// readMP3Duration reads the duration from an MP3 file (CBR or Xing VBR).
func readMP3Duration(r io.ReadSeeker) float64 {
	scanBuf := make([]byte, 4096)
	if _, err := r.Read(scanBuf); err != nil {
		return 0
	}
	for i := 0; i < len(scanBuf)-1; i++ {
		if scanBuf[i] != 0xFF || (scanBuf[i+1]&0xE0) != 0xE0 {
			continue
		}
		if i+3 >= len(scanBuf) {
			break
		}
		header := uint32(scanBuf[i])<<24 | uint32(scanBuf[i+1])<<16 |
			uint32(scanBuf[i+2])<<8 | uint32(scanBuf[i+3])
		version := int((header >> 19) & 0x03)
		sampleRate := mp3SampleRate(version, int((header>>10)&0x03))
		bitrate := mp3Bitrate(version, int((header>>12)&0x0F))
		padding := int((header >> 9) & 0x01)
		if sampleRate <= 0 || bitrate <= 0 {
			continue
		}
		var frameSize int
		if version == 3 {
			frameSize = (1440*bitrate/8)/sampleRate + padding
		} else {
			frameSize = (720*bitrate/8)/sampleRate + padding
		}
		if frameSize <= 0 {
			continue
		}
		samplesPerFrame := 1152
		if version != 3 {
			samplesPerFrame = 576
		}

		xingOffset := 36
		if version != 3 {
			xingOffset = 21
		}
		xingPos := i + xingOffset
		if xingPos+12 <= len(scanBuf) {
			if string(scanBuf[xingPos:xingPos+4]) == "Xing" ||
				string(scanBuf[xingPos:xingPos+4]) == "Info" {
				frameCount := uint32(scanBuf[xingPos+8])<<24 |
					uint32(scanBuf[xingPos+9])<<16 |
					uint32(scanBuf[xingPos+10])<<8 |
					uint32(scanBuf[xingPos+11])
				if frameCount > 0 {
					return float64(frameCount) * float64(samplesPerFrame) / float64(sampleRate)
				}
			}
		}

		fileSize, err := r.Seek(0, io.SeekEnd)
		if err != nil {
			return 0
		}
		audioStart := int64(0)
		if i > 10 && scanBuf[0] == 'I' && scanBuf[1] == 'D' && scanBuf[2] == '3' {
			audioStart = int64(scanBuf[9]&0x7F)<<21 | int64(scanBuf[10]&0x7F)<<14 |
				int64(scanBuf[7]&0x7F)<<7 | int64(scanBuf[6]&0x7F) + 10
		}
		audioEnd := fileSize
		if fileSize > 128 {
			if _, err := r.Seek(-128, io.SeekEnd); err != nil {
				return 0
			}
			tagBuf := make([]byte, 3)
			if _, err := r.Read(tagBuf); err == nil && string(tagBuf) == "TAG" {
				audioEnd = fileSize - 128
			}
		}
		audioBytes := audioEnd - audioStart
		if audioBytes <= 0 {
			return 0
		}
		totalFrames := audioBytes / int64(frameSize)
		return float64(totalFrames) * float64(samplesPerFrame) / float64(sampleRate)
	}
	return 0
}

func mp3Bitrate(version, bitrateIndex int) int {
	table := [3][14]int{
		{32000, 40000, 48000, 56000, 64000, 80000, 96000, 112000, 128000, 160000, 192000, 224000, 256000, 320000},
		{32000, 48000, 56000, 64000, 80000, 96000, 112000, 128000, 144000, 160000, 176000, 192000, 224000, 256000},
		{32000, 40000, 48000, 56000, 64000, 80000, 96000, 112000, 128000, 160000, 192000, 224000, 256000, 320000},
	}
	idx := version
	if idx < 0 || idx > 2 {
		idx = 0
	}
	if bitrateIndex < 1 || bitrateIndex > 14 {
		return 128000
	}
	return table[idx][bitrateIndex-1]
}

func mp3SampleRate(version, sampleRateIndex int) int {
	table := [3][3]int{
		{44100, 48000, 32000},
		{22050, 24000, 16000},
		{11025, 12000, 8000},
	}
	if version < 0 || version > 2 {
		version = 0
	}
	if sampleRateIndex < 0 || sampleRateIndex > 2 {
		return 44100
	}
	return table[version][sampleRateIndex]
}

func findBox(r io.ReadSeeker, name string) (int64, int64) {
	pos := int64(0)
	for {
		var size uint32
		if err := binary.Read(r, binary.BigEndian, &size); err != nil {
			return -1, -1
		}
		var boxType [4]byte
		if _, err := io.ReadFull(r, boxType[:]); err != nil {
			return -1, -1
		}
		boxSize := int64(size)
		if boxSize == 0 {
			fileEnd, _ := r.Seek(0, io.SeekEnd)
			boxSize = fileEnd - pos
			r.Seek(pos, io.SeekStart)
		} else if boxSize == 1 {
			var largeSize uint64
			if err := binary.Read(r, binary.BigEndian, &largeSize); err != nil {
				return -1, -1
			}
			boxSize = int64(largeSize)
		}
		if string(boxType[:]) == name {
			return pos, pos + boxSize
		}
		pos += boxSize
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			return -1, -1
		}
	}
}

func findBoxInReader(r io.Reader, name string) (int64, int64) {
	pos := int64(0)
	for {
		var size uint32
		if err := binary.Read(r, binary.BigEndian, &size); err != nil {
			return -1, -1
		}
		var boxType [4]byte
		if _, err := io.ReadFull(r, boxType[:]); err != nil {
			return -1, -1
		}
		boxSize := int64(size)
		if string(boxType[:]) == name {
			return pos, pos + boxSize
		}
		remaining := boxSize - 8
		if _, err := io.CopyN(io.Discard, r, remaining); err != nil {
			return -1, -1
		}
		pos += boxSize
	}
}
