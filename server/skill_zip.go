package server

import (
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// Go의 archive/zip은 Store(0)와 Deflate(8) 압축 해제만 기본 지원하며 다른 방식에서는
// "zip: unsupported compression algorithm"을 반환한다. 압축 프로그램은 기본값 외 설정에서
// 다른 방식(7-Zip의 bzip2, WinZip의 zstd)을 자주 사용하므로 순수 Go로 지원 가능한 두 방식을 추가한다.
// 해제할 수 없는 방식(Deflate64 / LZMA / XZ / PPMd / 암호화 파일)은 압축 해제 전에
// 하위 오류를 그대로 노출하는 대신 한국어로 안내한다.
const (
	zipMethodStore     = 0
	zipMethodDeflate   = 8
	zipMethodDeflate64 = 9
	zipMethodBzip2     = 12
	zipMethodLZMA      = 14
	zipMethodZstdPKW   = 20 // PKWARE가 초기에 zstd에 배정한 번호
	zipMethodZstd      = 93
	zipMethodXZ        = 95
	zipMethodJPEG      = 96
	zipMethodWavPack   = 97
	zipMethodPPMd      = 98
	zipMethodAES       = 99
)

var zipMethodNames = map[uint16]string{
	zipMethodStore:     "Store",
	zipMethodDeflate:   "Deflate",
	zipMethodDeflate64: "Deflate64",
	zipMethodBzip2:     "bzip2",
	zipMethodLZMA:      "LZMA",
	zipMethodZstdPKW:   "Zstandard",
	zipMethodZstd:      "Zstandard",
	zipMethodXZ:        "XZ",
	zipMethodJPEG:      "JPEG",
	zipMethodWavPack:   "WavPack",
	zipMethodPPMd:      "PPMd",
	zipMethodAES:       "AES 암호화",
}

func zipMethodName(m uint16) string {
	if n, ok := zipMethodNames[m]; ok {
		return n
	}
	return "알 수 없음"
}

// newSkillZipReader parses an uploaded archive and registers the extra decompressors
// we can support beyond the stdlib's Store/Deflate.
func newSkillZipReader(buf []byte) (*zip.Reader, error) {
	zr, err := zip.NewReader(bytes.NewReader(buf), int64(len(buf)))
	if err != nil {
		return nil, fmt.Errorf("압축 파일을 해석할 수 없습니다(zip 형식 필요): %w", err)
	}
	zr.RegisterDecompressor(zipMethodBzip2, func(r io.Reader) io.ReadCloser {
		return io.NopCloser(bzip2.NewReader(r))
	})
	zdec := zstd.ZipDecompressor(zstd.WithDecoderConcurrency(1))
	zr.RegisterDecompressor(zipMethodZstd, zdec)
	zr.RegisterDecompressor(zipMethodZstdPKW, zdec)
	return zr, nil
}

// skillZipEntry pairs a zip entry with its decoded (UTF-8) name — f.Name may hold
// raw GBK bytes, see zipEntryName.
type skillZipEntry struct {
	f    *zip.File
	name string
}

// skillZipEntries lists the archive's real files (no directory entries, no archiver
// junk) with their names decoded to UTF-8.
func skillZipEntries(zr *zip.Reader) []skillZipEntry {
	out := make([]skillZipEntry, 0, len(zr.File))
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := zipEntryName(f)
		if strings.HasPrefix(name, "__MACOSX/") || strings.Contains(name, "/__MACOSX/") ||
			path.Base(name) == ".DS_Store" {
			continue // macOS 압축 시 남은 파일
		}
		out = append(out, skillZipEntry{f: f, name: name})
	}
	return out
}

// zipEntryName은 항목 경로를 UTF-8로 반환한다. Windows의 7-Zip/WinRAR/탐색기는
// UTF-8 플래그 없이 중국어 파일명을 GBK로 저장할 수 있다. Go가 이 바이트를 그대로 보존하면
// 유효한 UTF-8도 아니고 경로 검증도 통과하지 못하므로 GBK 디코딩을 대체 경로로 사용한다.
func zipEntryName(f *zip.File) string {
	if utf8.ValidString(f.Name) {
		return f.Name
	}
	if dec, err := simplifiedchinese.GBK.NewDecoder().String(f.Name); err == nil && utf8.ValidString(dec) {
		return dec
	}
	return f.Name
}

// checkSkillZipMethods rejects archives we cannot extract, naming the offending
// entry and method instead of letting f.Open() fail with an opaque English error.
func checkSkillZipMethods(entries []skillZipEntry) error {
	for _, e := range entries {
		if e.f.Flags&0x1 != 0 || e.f.Method == zipMethodAES {
			return fmt.Errorf("압축 파일이 암호화되어 있습니다(%s). 암호화하지 않은 zip을 업로드하세요", e.name)
		}
		switch e.f.Method {
		case zipMethodStore, zipMethodDeflate, zipMethodBzip2, zipMethodZstd, zipMethodZstdPKW:
		default:
			return fmt.Errorf("압축 파일이 지원하지 않는 압축 방식 %s(method %d)을 사용합니다: %s. "+
				"저장 또는 Deflate 방식으로 다시 압축하세요(7-Zip/WinRAR에서 Deflate 선택, "+
				"운영체제의 압축/압축 폴더로 보내기 기능 또는 zip -r 명령 사용)",
				zipMethodName(e.f.Method), e.f.Method, e.name)
		}
	}
	return nil
}
