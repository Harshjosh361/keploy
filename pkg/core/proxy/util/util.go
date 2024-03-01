package util

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"go.keploy.io/server/v2/utils"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	// "math/rand"
	"net"
	"strconv"
	"strings"
)

var sendLogs = true

// idCounter is used to generate random ID for each request
var idCounter int64 = -1

func GetNextID() int64 {
	return atomic.AddInt64(&idCounter, 1)
}

// ReadBuffConn is used to read the buffer from the connection
func ReadBuffConn(ctx context.Context, logger *zap.Logger, conn net.Conn, bufferChannel chan []byte, errChannel chan error) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			if conn == nil {
				logger.Debug("the conn is nil")
			}
			buffer, err := ReadBytes(ctx, conn)
			if err != nil {
				logger.Error("failed to read the packet message in proxy", zap.Error(err))
				errChannel <- err
				return
			}
			bufferChannel <- buffer
		}
	}
}

func ReadInitialBuf(ctx context.Context, logger *zap.Logger, conn net.Conn) ([]byte, error) {
	readErr := errors.New("failed to read the initial request buffer")

	initialBuf, err := ReadBytes(ctx, conn)
	if err != nil && err != io.EOF {
		logger.Error("failed to read the request message in proxy", zap.Error(err))
		return nil, readErr
	}

	if err == io.EOF && len(initialBuf) == 0 {
		logger.Debug("received EOF, closing conn", zap.Error(err))
		return nil, readErr
	}

	logger.Debug("received initial buffer", zap.Any("size", len(initialBuf)), zap.Any("initial buffer", initialBuf))
	if err != nil {
		logger.Error("failed to read the request message in proxy", zap.Error(err))
		return nil, readErr
	}
	return initialBuf, nil
}

// ReadBytes function is utilized to read the complete message from the reader until the end of the file (EOF).
// It returns the content as a byte array.
func ReadBytes(ctx context.Context, reader io.Reader) ([]byte, error) {
	var buffer []byte
	const maxEmptyReads = 5
	emptyReads := 0

	for {
		select {
		case <-ctx.Done():
			return buffer, nil
		default:
			buf := make([]byte, 1024)
			n, err := reader.Read(buf)

			if n > 0 {
				buffer = append(buffer, buf[:n]...)
				emptyReads = 0 // reset the counter because we got some data
			}

			if err != nil {
				if err == io.EOF {
					emptyReads++
					if emptyReads >= maxEmptyReads {
						return buffer, err // multiple EOFs in a row, probably a true EOF
					}
					time.Sleep(time.Millisecond * 100) // sleep before trying again
					continue
				}
				return buffer, err
			}

			if n < len(buf) {
				return buffer, nil
			}
		}
	}
}

// ReadRequiredBytes ReadBytes function is utilized to read the complete message from the reader until the end of the file (EOF).
// It returns the content as a byte array.
func ReadRequiredBytes(ctx context.Context, reader io.Reader, numBytes int) ([]byte, error) {
	var buffer []byte
	const maxEmptyReads = 5
	emptyReads := 0

	for {
		select {
		case <-ctx.Done():
			return buffer, nil
		default:
			buf := make([]byte, numBytes)

			n, err := reader.Read(buf)

			if n == numBytes {
				buffer = append(buffer, buf...)
				return buffer, nil
			}

			if n > 0 {
				buffer = append(buffer, buf[:n]...)
				numBytes = numBytes - n
				emptyReads = 0 // reset the counter because we got some data
			}

			if err != nil {
				if err == io.EOF {
					emptyReads++
					if emptyReads >= maxEmptyReads {
						return buffer, err // multiple EOFs in a row, probably a true EOF
					}
					time.Sleep(time.Millisecond * 100) // sleep before trying again
					continue
				}
				return buffer, err
			}
		}
	}
}

// PassThrough function is used to pass the network traffic to the destination connection.
// It also closes the destination connection if the function returns an error.
func PassThrough(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, requestBuffer [][]byte) ([]byte, error) {

	if destConn == nil {
		return nil, errors.New("failed to pass network traffic to the destination conn")
	}

	defer func(destConn net.Conn) {
		err := destConn.Close()
		if err != nil {
			logger.Error("failed to close the destination connection", zap.Error(err))
		}
	}(destConn)

	logger.Debug("trying to forward requests to target", zap.Any("Destination Addr", destConn.RemoteAddr().String()))
	for _, v := range requestBuffer {
		_, err := destConn.Write(v)
		if err != nil {
			logger.Error("failed to write request message to the destination server", zap.Error(err), zap.Any("Destination Addr", destConn.RemoteAddr().String()))
			return nil, err
		}
	}

	// channels for writing messages from proxy to destination or client
	destBufferChannel := make(chan []byte)
	errChannel := make(chan error)

	go func() {
		defer utils.Recover(logger)
		ReadBuffConn(ctx, logger, destConn, destBufferChannel, errChannel)
	}()

	select {
	case buffer := <-destBufferChannel:
		// Write the response message to the client
		_, err := clientConn.Write(buffer)
		if err != nil {
			logger.Error("failed to write response to the client", zap.Error(err))
			return nil, err
		}

		logger.Debug("the iteration for the generic response ends with responses:"+strconv.Itoa(len(buffer)), zap.Any("buffer", buffer))
	case err := <-errChannel:
		if netErr, ok := err.(net.Error); !(ok && netErr.Timeout()) && err != nil {
			return nil, err
		}
		return nil, nil

	case <-ctx.Done():
		return nil, nil
	}

	return nil, nil
}

// ToIP4AddressStr converts the integer IP4 Address to the octet format
func ToIP4AddressStr(ip uint32) string {
	// convert the IP address to a 32-bit binary number
	ipBinary := fmt.Sprintf("%032b", ip)

	// divide the binary number into four 8-bit segments
	firstByte, _ := strconv.ParseUint(ipBinary[0:8], 2, 64)
	secondByte, _ := strconv.ParseUint(ipBinary[8:16], 2, 64)
	thirdByte, _ := strconv.ParseUint(ipBinary[16:24], 2, 64)
	fourthByte, _ := strconv.ParseUint(ipBinary[24:32], 2, 64)

	// concatenate the four decimal segments with a dot separator to form the dot-decimal string
	return fmt.Sprintf("%d.%d.%d.%d", firstByte, secondByte, thirdByte, fourthByte)
}

func ToIPv6AddressStr(ip [4]uint32) string {
	// construct a byte slice
	ipBytes := make([]byte, 16) // IPv6 address is 128 bits or 16 bytes long
	for i := 0; i < 4; i++ {
		// for each uint32, extract its four bytes and put them into the byte slice
		ipBytes[i*4] = byte(ip[i] >> 24)
		ipBytes[i*4+1] = byte(ip[i] >> 16)
		ipBytes[i*4+2] = byte(ip[i] >> 8)
		ipBytes[i*4+3] = byte(ip[i])
	}
	// net.IP is a byte slice, so it can be directly used to construct an IPv6 address
	ipv6Addr := net.IP(ipBytes)
	return ipv6Addr.String()
}

func GetLocalIPv4() (net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			return nil, err
		}

		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if ok && !ipNet.IP.IsLoopback() && ipNet.IP.To4() != nil {
				return ipNet.IP, nil
			}
		}
	}

	return nil, fmt.Errorf("no valid IP address found")
}

func ToIPV4(ip net.IP) (uint32, bool) {
	ipv4 := ip.To4()
	if ipv4 == nil {
		return 0, false // Return 0 or handle the error accordingly
	}

	return uint32(ipv4[0])<<24 | uint32(ipv4[1])<<16 | uint32(ipv4[2])<<8 | uint32(ipv4[3]), true
}

func GetLocalIPv6() (net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			return nil, err
		}

		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if ok && !ipNet.IP.IsLoopback() && ipNet.IP.To4() == nil && ipNet.IP.To16() != nil {
				return ipNet.IP, nil
			}
		}
	}

	return nil, fmt.Errorf("no valid IPv6 address found")
}

func IPv6ToUint32Array(ip net.IP) ([4]uint32, error) {
	ip = ip.To16()
	if ip == nil {
		return [4]uint32{}, errors.New("invalid IPv6 address")
	}

	return [4]uint32{
		binary.BigEndian.Uint32(ip[0:4]),
		binary.BigEndian.Uint32(ip[4:8]),
		binary.BigEndian.Uint32(ip[8:12]),
		binary.BigEndian.Uint32(ip[12:16]),
	}, nil
}

func IPToDotDecimal(ip net.IP) string {
	ipStr := ip.String()
	if ip.To4() != nil {
		ipStr = ip.To4().String()
	}
	return ipStr
}

func IsDirectoryExist(path string) bool {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false
	}
	return info.IsDir()
}

func IsJava(input string) bool {
	// Convert the input string and the search term "java" to lowercase for a case-insensitive comparison.
	inputLower := strings.ToLower(input)
	searchTerm := "java"
	searchTermLower := strings.ToLower(searchTerm)

	// Use strings.Contains to check if the lowercase input contains the lowercase search term.
	return strings.Contains(inputLower, searchTermLower)
}

// IsJavaInstalled checks if java is installed on the system
func IsJavaInstalled() bool {
	_, err := exec.LookPath("java")
	return err == nil
}

// GetJavaHome returns the JAVA_HOME path
func GetJavaHome(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "java", "-XshowSettings:properties", "-version")
	var out bytes.Buffer
	cmd.Stderr = &out // The output we need is printed to STDERR

	if err := cmd.Run(); err != nil {
		return "", err
	}

	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "java.home") {
			parts := strings.Split(line, "=")
			if len(parts) > 1 {
				return strings.TrimSpace(parts[1]), nil
			}
		}
	}

	return "", fmt.Errorf("java.home not found in command output")
}
