package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"io"
	"math"
	"math/rand"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alecthomas/kong"
	"github.com/crazy3lf/colorconv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"golang.org/x/sync/semaphore"
)

const (
	programName = "pixelkanone"
	programDesc = "pixelflut Go client"
)

type verboseFlag bool

func (v verboseFlag) BeforeApply() error {
	log.Logger = log.Level(zerolog.DebugLevel)
	return nil
}

type traceFlag bool

func (v traceFlag) BeforeApply() error {
	log.Logger = log.Logger.With().Caller().Logger().Level(zerolog.TraceLevel)
	return nil
}

type rootCmd struct {
	// Global options
	Verbose verboseFlag `help:"Enable verbose mode, implies log"`
	Trace   traceFlag   `hidden:""`
	Server  string      `help: "server"`
	Port    int16       `help: "port"`
	Xoff    uint
	Yoff    uint
	Width   uint
	Height  uint
	Conns   uint
}

func RunCommandLineTool() int {
	// add info about build to description
	desc := programDesc + " (" + runtime.GOARCH + ")"

	// set global log level to trace so individual loggers can be set to all levels
	zerolog.SetGlobalLevel(zerolog.TraceLevel)

	// set default global logger log level before kong possibly overrides it
	log.Logger = log.Level(zerolog.InfoLevel)

	// Dynamically build Kong options
	options := []kong.Option{
		kong.Name(programName),
		kong.Description(desc),
		kong.UsageOnError(),
		kong.ConfigureHelp(kong.HelpOptions{
			Compact: true,
			Summary: true,
		}),
	}

	// Parse common cli options
	var cli rootCmd
	kong.Parse(&cli, options...)

	// tell who we are
	log.Debug().Msg(desc)

	shoot(cli.Server, cli.Port, cli.Xoff, cli.Yoff, cli.Width, cli.Height, 4, cli.Conns)
	return 0
}

func writeCmd(w io.Writer, cmd string) error {
	if _, err := w.Write([]byte(cmd + "\n")); err != nil {
		return fmt.Errorf("write message: %w", err)
	}

	return nil
}

func readReply(r io.Reader) (string, error) {
	off := 0
	buf := make([]byte, 1024)
	for {
		n, err := r.Read(buf[off:])
		if err != nil {
			return "", err
		}

		if n > 0 {
			if buf[n-1] == 0x0a {
				return string(buf[0 : n-1]), nil
			}
			off += n
		}
	}
}

func size(conn net.Conn) (int, int, error) {
	err := writeCmd(conn, "SIZE")
	if err != nil {
		return 0, 0, fmt.Errorf("send SIZE: %w", err)
	}

	reply, err := readReply(conn)
	if err != nil {
		return 0, 0, fmt.Errorf("read SIZE: %w", err)
	}

	size := strings.Split(reply, " ")
	if len(size) != 3 {
		return 0, 0, errors.New("unexpected reply: " + reply)
	}

	if size[0] != "SIZE" {
		return 0, 0, errors.New("unexpected reply: " + reply)
	}

	x, err := strconv.Atoi(size[1])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid x value: %s", reply)
	}

	y, err := strconv.Atoi(size[2])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid y value: %s", reply)
	}

	return x, y, nil
}

func pixel(w io.Writer, x, y int, r, g, b, a byte) {
	if a == 255 {
		writeCmd(w, fmt.Sprintf("PX %d %d %02x%02x%02x", x, y, r, g, b))
	} else {
		writeCmd(w, fmt.Sprintf("PX %d %d %02x%02x%02x%02x", x, y, r, g, b, a))
	}
}

type connResponse struct {
	conn net.Conn
	err  error
}

func multiconnect(ctx context.Context, connCh chan connResponse, connSem *semaphore.Weighted, connectionString string, timeoutSecs, threads uint) {
	defer close(connCh)
	connMuxCh := make(chan connResponse)
	defer close(connCh)

	// sub-ctx to cancel our connecting threads and wg to wait for them to exit
	cancelCtx, cancelFunc := context.WithCancel(ctx)
	defer cancelFunc()
	var wg sync.WaitGroup

	// launch connection in go routine to hammer the server until it accepts a conn
	// we often get timeouts but we don't want to wait so long...
	// each thread can open a number of connectons and will try to open new ones until connection
	// limit is reached
	for i := 0; i < int(threads); i++ {
		wg.Add(1)
		go func(ch chan connResponse, sem *semaphore.Weighted, thNum int) {
			defer wg.Done()
			for {
				if err := sem.Acquire(ctx, 1); err != nil {
					log.Err(err).Msg("acquire semaphore")
					connMuxCh <- connResponse{nil, err}
					return
				}

				// repeat when we get a timeout, return error or conn otherwise
				conn, err := net.DialTimeout("tcp", connectionString, time.Second*time.Duration(timeoutSecs))
				if err != nil {
					var timeoutError net.Error
					if !(errors.As(err, &timeoutError) && timeoutError.Timeout()) && !os.IsTimeout(err) {
						ch <- connResponse{nil, err}
						return
					} else {
						sem.Release(1)
						continue
					}
				}

				select {
				case <-cancelCtx.Done():
					return
				case ch <- connResponse{conn, nil}:
					log.Info().Int("thread", thNum).Msg("CONNECTED")
				}
			}
		}(connMuxCh, connSem, i)
	}

	defer wg.Wait()

	for {
		select {
		case <-ctx.Done():
			log.Debug().Err(ctx.Err()).Msg("multiconnect done")
			return

		case response := <-connMuxCh:
			err := response.err
			if err != nil {
				log.Error().Err(err).Msg("connecting " + connectionString)
				connCh <- connResponse{nil, err}
				return
			}
			connCh <- connResponse{response.conn, nil}
		}
	}
}

func render(pixBuf *[][]byte, xoff, yoff, width, height, numClients uint, hoff float64) {
	h := 0.0
	increment := (2.0 * math.Pi) / float64(width)
	off := (hoff * 2.0 * math.Pi)
	var x, y uint
	var segment uint
	for segment = 0; segment < uint(numClients); segment++ {
		var sb strings.Builder
		for x = segment * (width / numClients); x < ((segment + 1) * (width / numClients)); x++ {
			for y = 0; y < height; y++ {
				sinVal := (increment * float64(x)) + off
				h = (180.0 * math.Sin(sinVal)) + 180.0
				r, g, b, _ := colorconv.HSVToRGB(h, 1.0, 1.0)
				fmt.Fprintf(&sb, "PX %d %d %02x%02x%02x\n", x+xoff, y+yoff, r, g, b)
			}
		}
		(*pixBuf)[segment] = []byte(sb.String())
	}
}

func renderLab(pixBuf *[][]byte, img []byte, iw, ih, xoff, yoff, width, height, numClients uint, hoff float64) {
	//h := 0.0
	//increment := (2.0 * math.Pi) / float64(width)
	//off := (hoff * 2.0 * math.Pi)
	//var x, y uint
	var segment uint
	for segment = 0; segment < uint(numClients); segment++ {
		var sb strings.Builder
		/*for x = segment * (width / numClients); x < ((segment + 1) * (width / numClients)); x++ {
			for y = 0; y < height; y++ {
				sinVal := (increment * float64(x)) + off
				h = (180.0 * math.Sin(sinVal)) + 180.0
				r, g, b, _ := colorconv.HSVToRGB(h, 1.0, 1.0)
				fmt.Fprintf(&sb, "PX %d %d %02x%02x%02x\n", x+xoff, y+yoff, r, g, b)
			}
		}*/
		segw := width / 2
		segx := xoff + segw*segment
		for i := 0; i < 16; i++ {
			fmt.Fprintf(&sb, "OFFSET %d %d\n", int(segx+(uint(rand.Uint32())%(width-iw))), int(yoff+(uint(rand.Uint32())%(height-ih))))

			for ix := 0; ix < int(iw); ix++ {
				for iy := 0; iy < int(ih); iy++ {
					a := img[iy*int(iw)+ix]
					if a > 0 {
						fmt.Fprintf(&sb, "PX %d %d FF\n", ix, iy)
					}
				}
			}
		}
		(*pixBuf)[segment] = []byte(sb.String())
	}
}

func shoot(server string, port int16, xoff, yoff, width, height, threads, maxConns uint) {
	connectionString := server + ":" + strconv.Itoa(int(port))
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()

	var writerBufIdx int32
	atomic.StoreInt32(&writerBufIdx, 0)

	pixBufs := make([][][]byte, 2)
	go func(bufs [][][]byte, bufIdx *int32) {
		imgFile, err := os.ReadFile("lab.png")
		if err != nil {
			log.Err(err)
		}
		img, err := png.Decode(bytes.NewReader(imgFile))
		if err != nil {
			log.Err(err)
		}

		imax := img.Bounds().Max
		imgBuf := make([]byte, imax.X*imax.Y)
		for x := 0; x < imax.X; x++ {
			for y := 0; y < imax.Y; y++ {
				_, _, _, a := img.At(x, y).RGBA()
				var b byte
				if a > 0 {
					b = 255
				}
				imgBuf[y*int(imax.X)+x] = b
			}
		}

		pixBuf := make([][]byte, maxConns)
		hoff := 0.0
		renderLab(&pixBuf, imgBuf, uint(imax.X), uint(imax.Y), xoff, yoff, width, height, maxConns, hoff)
		bufs[atomic.LoadInt32(bufIdx)] = pixBuf
		log.Info().Int("sz", len(bufs[0][0])).Msg("prepared buffer")

		for {
			_idx := atomic.LoadInt32(bufIdx)
			atomic.StoreInt32(bufIdx, _idx^1)
			pixBuf = make([][]byte, maxConns)
			renderLab(&pixBuf, imgBuf, uint(imax.X), uint(imax.Y), xoff, yoff, width, height, maxConns, hoff)
			bufs[_idx] = pixBuf
			time.Sleep(time.Millisecond * 32)
			hoff += 0.005
			hoff = math.Mod(hoff, 1.0)
		}
	}(pixBufs, &writerBufIdx)

	time.Sleep(time.Second)

	// use a semaphore to track connection count
	sem := semaphore.NewWeighted(int64(maxConns))

	// launch connecting threads
	connCh := make(chan connResponse)
	go multiconnect(ctx, connCh, sem, connectionString, 1, threads)

	// get fresh connections
	writerNum := uint(0)
	for {
		select {
		case response := <-connCh:
			if response.err != nil {
				// just quit all and let deferred function handle cleanup
				return
			}

			// launch writer thread
			go func(conn net.Conn, pixbuf [][][]byte, bufIdx *int32, writerNum uint) {
				defer sem.Release(1)

				// todo: the first thread that runs should query this once
				/*
					maxx, maxy, err := size(conn)
					if err != nil {
						log.Err(err).Msg("error getting size")
						return
					}
					log.Info().Int("x", maxx).Int("y", maxy).Msg("canvas size")
				*/
				for {
					_, err := conn.Write(pixbuf[atomic.LoadInt32(bufIdx)][writerNum])
					if err != nil {
						log.Err(err)
						return
					}
				}
			}(response.conn, pixBufs, &writerBufIdx, writerNum)
			writerNum++
			writerNum = writerNum % maxConns
		}
	}
}
