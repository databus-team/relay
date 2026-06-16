package protocol

import (
	"sync"

	"github.com/klauspost/compress/zstd"
)

var (
	encoder     *zstd.Encoder
	decoder     *zstd.Decoder
	encoderOnce sync.Once
	decoderOnce sync.Once
)

func getEncoder() *zstd.Encoder {
	encoderOnce.Do(func() {
		encoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	})
	return encoder
}

func getDecoder() *zstd.Decoder {
	decoderOnce.Do(func() {
		decoder, _ = zstd.NewReader(nil)
	})
	return decoder
}

func Compress(src []byte) []byte {
	return getEncoder().EncodeAll(src, nil)
}

func Decompress(src []byte) ([]byte, error) {
	return getDecoder().DecodeAll(src, nil)
}
