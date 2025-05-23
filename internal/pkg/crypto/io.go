// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License 2.0;
// you may not use this file except in compliance with the Elastic License 2.0.

package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Option is the default options used to generate the encrypt and decrypt writer.
// NOTE: the defined options need to be same for both the Reader and the writer.
type Option struct {
	Generator       bytesGen
	IterationsCount int
	KeyLength       int
	SaltLength      int
	IVLength        int

	// BlockSize must be a factor of aes.BlockSize
	BlockSize int
}

// DefaultOptions is the default options to use when creating the writer, changing might decrease
// the efficacity of the encryption.
var DefaultOptions = &Option{
	IterationsCount: 10000,
	KeyLength:       32,
	SaltLength:      64,
	IVLength:        12,
	Generator:       randomBytes,
	BlockSize:       bytes.MinRead,
}

// versionMagicHeader is the format version that will be added at the beginning of the header and
// can be used to change how the decryption work in future version.
var versionMagicHeader = []byte("v2")

// Writer is an io.Writer implementation that will encrypt any data that it need to write, before
// writing any data to the wrapped writer it will lazy write an header with the necessary information
// to be able to decrypt the data.
type Writer struct {
	option    *Option
	password  []byte
	writer    io.Writer
	generator bytesGen

	// internal
	wroteHeader bool
	err         error
	gcm         cipher.AEAD
	salt        []byte
}
type bytesGen func(int) ([]byte, error)

// NewWriter returns a new encrypted Writer.
func NewWriter(writer io.Writer, password []byte, option *Option) (*Writer, error) {
	if err := option.Validate(); err != nil {
		return nil, err
	}

	var g bytesGen
	if option.Generator == nil {
		g = randomBytes
	} else {
		g = option.Generator
	}

	salt, err := g(option.SaltLength)
	if err != nil {
		return nil, fmt.Errorf("fail to generate random password salt: %w", err)
	}

	return &Writer{
		option:    option,
		password:  password,
		generator: g,
		writer:    writer,
		salt:      salt,
	}, nil
}

// NewWriterWithDefaults create a new encryption writer with the defaults options.
func NewWriterWithDefaults(writer io.Writer, password []byte) (*Writer, error) {
	return NewWriter(writer, password, DefaultOptions)
}

// Write takes a byte slice and encrypt to the destination writer, it will return any errors when
// generating the header information or when we try to encode the data.
func (w *Writer) Write(b []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}

	if !w.wroteHeader {
		w.wroteHeader = true

		// Stretch the user provided key.
		passwordBytes, err := stretchPassword(
			w.password,
			w.salt,
			w.option.IterationsCount,
			w.option.KeyLength,
		)
		if err != nil {
			return 0, fmt.Errorf("failed to stretch password: %w", err)
		}

		// Select AES-256: because len(passwordBytes) == 32 bytes.
		block, err := aes.NewCipher(passwordBytes)
		if err != nil {
			w.err = fmt.Errorf("could not create the cipher to encrypt: %w", err)
			return 0, w.err
		}

		aesgcm, err := cipher.NewGCM(block)
		if err != nil {
			w.err = fmt.Errorf("could not create the GCM to encrypt: %w", err)
			return 0, w.err
		}

		w.gcm = aesgcm

		// Write headers
		// VERSION|SALT|IV|PAYLOAD
		header := new(bytes.Buffer)
		header.Write(versionMagicHeader)
		header.Write(w.salt)

		n, err := w.writer.Write(header.Bytes())
		if err != nil {
			w.err = fmt.Errorf("fail to write encoding information header: %w", err)
			return 0, w.err
		}

		if n != len(header.Bytes()) {
			w.err = errors.New("written bytes do not match header size")
		}

		if err := w.writeBlock(b); err != nil {
			return 0, fmt.Errorf("fail to write block: %w", err)
		}

		return len(b), err
	}

	if err := w.writeBlock(b); err != nil {
		return 0, fmt.Errorf("fail to write block: %w", err)
	}

	return len(b), nil
}

func (w *Writer) writeBlock(b []byte) error {
	// randomly generate the salt and the initialization vector, this information will be saved
	// on disk in the file as part of the header
	iv, err := w.generator(w.option.IVLength)
	if err != nil {
		w.err = fmt.Errorf("fail to generate random IV: %w", err)
		return w.err
	}

	//nolint:errcheck // Ignore the error at this point.
	w.writer.Write(iv)

	encodedBytes := w.gcm.Seal(nil, iv, b, nil)

	l := make([]byte, 4)
	binary.LittleEndian.PutUint32(l, uint32(len(encodedBytes))) //nolint:gosec // ignoring unsafe type conversion
	//nolint:errcheck // Ignore the error at this point.
	w.writer.Write(l)

	_, err = w.writer.Write(encodedBytes)
	if err != nil {
		return fmt.Errorf("fail to encode data: %w", err)
	}

	return nil
}

// Reader implements the io.Reader interface and allow to decrypt bytes from the Writer. The reader
// will lazy read the header from the wrapper reader to initialize everything required to decrypt
// the data.
type Reader struct {
	option   *Option
	password []byte
	reader   io.Reader

	// internal
	err        error
	readHeader bool
	gcm        cipher.AEAD
	buf        []byte
	eof        bool
}

// NewReader returns a new decryption reader.
func NewReader(reader io.Reader, password []byte, option *Option) (*Reader, error) {
	if reader == nil {
		return nil, errors.New("missing reader")
	}

	return &Reader{
		option:   option,
		password: password,
		reader:   reader,
	}, nil
}

// NewReaderWithDefaults create a decryption io.Reader with the default options.
func NewReaderWithDefaults(reader io.Reader, password []byte) (*Reader, error) {
	return NewReader(reader, password, DefaultOptions)
}

// Read reads the bytes from a wrapped io.Reader and will decrypt the content on the fly.
func (r *Reader) Read(b []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}

	// Lets read the header.
	if !r.readHeader {
		r.readHeader = true
		vLen := len(versionMagicHeader)
		buf := make([]byte, vLen+r.option.SaltLength)
		n, err := io.ReadAtLeast(r.reader, buf, len(buf))
		if err != nil {
			r.err = fmt.Errorf("fail to read encoding header: %w", err)
			return n, err
		}

		v := buf[0:vLen]
		if !bytes.Equal(versionMagicHeader, v) {
			return 0, fmt.Errorf("unknown version %s (%+v)", string(v), v)
		}

		salt := buf[vLen : vLen+r.option.SaltLength]

		// Stretch the user provided key.
		passwordBytes, err := stretchPassword(
			r.password,
			salt,
			r.option.IterationsCount,
			r.option.KeyLength,
		)
		if err != nil {
			return 0, fmt.Errorf("failed to stretch password: %w", err)
		}

		block, err := aes.NewCipher(passwordBytes)
		if err != nil {
			r.err = fmt.Errorf("could not create the cipher to decrypt the data: %w", err)
			return 0, r.err
		}

		aesgcm, err := cipher.NewGCM(block)
		if err != nil {
			r.err = fmt.Errorf("could not create the GCM to decrypt the data: %w", err)
			return 0, r.err
		}
		r.gcm = aesgcm
	}

	return r.readTo(b)
}

func (r *Reader) readTo(b []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}

	if !r.eof {
		if err := r.consumeBlock(); err != nil {
			// We read all the blocks
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				r.eof = true
			} else {
				r.err = err
				return 0, err
			}
		}
	}

	n := copy(b, r.buf)
	r.buf = r.buf[n:]

	if r.eof && len(r.buf) == 0 {
		r.err = io.EOF
	}

	return n, r.err
}

func (r *Reader) consumeBlock() error {
	// Retrieve block information:
	// - Initialization vector
	// - Length of the block
	iv, l, err := r.readBlockInfo()
	if err != nil {
		return err
	}

	encodedBytes := make([]byte, l)
	_, err = io.ReadAtLeast(r.reader, encodedBytes, l)
	if err != nil {
		r.err = fmt.Errorf("fail read the block of %d bytes: %w", l, err)
	}

	decodedBytes, err := r.gcm.Open(nil, iv, encodedBytes, nil)
	if err != nil {
		return fmt.Errorf("fail to decode bytes: %w", err)
	}
	r.buf = append(r.buf[:], decodedBytes...)

	return nil
}

func (r *Reader) readBlockInfo() ([]byte, int, error) {
	buf := make([]byte, r.option.IVLength+4)
	_, err := io.ReadAtLeast(r.reader, buf, len(buf))
	if err != nil {
		return nil, 0, err
	}

	iv := buf[0:r.option.IVLength]
	l := binary.LittleEndian.Uint32(buf[r.option.IVLength:])

	return iv, int(l), nil
}

// Close will propagate the Close call to the wrapped reader.
func (r *Reader) Close() error {
	a, ok := r.reader.(io.ReadCloser)
	if ok {
		return a.Close()
	}
	return nil
}

func randomBytes(length int) ([]byte, error) {
	r := make([]byte, length)
	_, err := rand.Read(r)
	if err != nil {
		return nil, err
	}

	return r, nil
}

func stretchPassword(password, salt []byte, c, kl int) ([]byte, error) {
	return pbkdf2.Key(sha512.New, string(password), salt, c, kl)
}
