package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/kong"
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

func min3(a, b, c float64) float64 {
	return math.Min(math.Min(a, b), c)
}

func hueToRGB(h float64) (int, int, int) {
	kr := math.Mod(5+h*6, 6)
	kg := math.Mod(3+h*6, 6)
	kb := math.Mod(1+h*6, 6)

	r := 1 - math.Max(min3(kr, 4-kr, 1), 0)
	g := 1 - math.Max(min3(kg, 4-kg, 1), 0)
	b := 1 - math.Max(min3(kb, 4-kb, 1), 0)

	return int(math.Round(r * 255.0)), int(math.Round(g * 255.0)), int(math.Round(b * 255.0))
}

func shoot(server string, port int16, xoff, yoff, width, height, threads, maxConns uint) {
	connectionString := server + ":" + strconv.Itoa(int(port))
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()

	pixbufs := make([][2][]byte, maxConns)
	for connBufNum := 0; connBufNum < int(maxConns); connBufNum++ {
		var r, g, b int
		h := 0.0
		var pixbuf []byte
		for x := xoff; x < (xoff + width); x++ {
			for y := yoff; y < (yoff + height); y++ {
				h = (1.0 / float64(width)) * float64(x)
				r, g, b = hueToRGB(h)
				pixbuf = append(pixbuf, ([]byte(fmt.Sprintf("PX %d %d %02x%02x%02x\n", x, y, r, g, b)))...)
			}
		}
		log.Info().Uint("num", uint(connBufNum)).Int("sz", len(pixbuf)).Msg("prepared buffer")
		pixbufs[connBufNum][0] = pixbuf
	}

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
			go func(conn net.Conn, pixbuf [2][]byte, writerNum, xoff, yoff, width, height uint) {
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

				log.Info().Uint("num", writerNum).Uint("x", xoff).Uint("y", yoff).Uint("w", width).Uint("h", height).Msg("blasting rect")
				for {
					conn.Write(pixbuf[0])
					_, err := conn.Write(pixbuf[0])
					if err != nil {
						log.Err(err)
						return
					}
				}
			}(response.conn, pixbufs[writerNum], writerNum, xoff+((width/maxConns)*writerNum), yoff, width/maxConns, height)
			writerNum++
			writerNum = writerNum % maxConns
		}
	}
}
