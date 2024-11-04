package main

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/textproto"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func main() {
	if err := realmain(); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

type sessionState int

const (
	beforeEHLO sessionState = iota
	beforeMAIL
	beforeRCPT
	beforeDATA
	inDATA
	afterDATA
)

type session struct {
	client string
	state  sessionState
	tx     *transaction
}

type transaction struct {
	mailFrom string
	rcptTo   []string
	data     []byte
}

func realmain() error {
	var serverName string

	rootCmd := &cobra.Command{
		Use:   "go-smts-sink",
		Short: "go-smtp-sink is a SMTP Sink server written in Go.",
		Run: func(cmd *cobra.Command, args []string) {
			if len(args) < 1 {
				cmd.PrintErr("please specify an address to listen to\n")
				return
			}

			addr := args[0]

			slog.Info(fmt.Sprintf("Listening to %s...", addr))

			srv := &server{hostname: serverName}

			l, err := net.Listen("tcp", addr)
			if err != nil {
				slog.Error("Failed to listen", "error", err.Error())
				return
			}

			defer l.Close()

			for {
				func() {
					conn, err := l.Accept()
					if err != nil {
						slog.Error("Failed to accept", "error", err.Error())
						return
					}

					defer conn.Close()

					srv.serveConn(conn)
				}()
			}
		},
	}

	rootCmd.Flags().StringVar(
		&serverName,
		"server-name",
		"mx.example.com",
		"specify a server name",
	)

	return rootCmd.Execute()
}

type server struct {
	hostname string
}

func (s *server) serveConn(conn net.Conn) {
	br := bufio.NewReader(conn)
	bw := bufio.NewWriter(conn)

	writeReplyAndFlush(bw, 220, fmt.Sprintf("%s ESMTP", s.hostname))

	sess := &session{}

	var quit bool
	for !quit {
		verb, args, err := readCommand(br)

		if err != nil {
			slog.Error("Failed to read the command", "error", err.Error())
			writeReplyAndFlush(bw, 550, "Requested action not taken")
			quit = true
			break
		}
		// TODO:
		//  DATA

		// DONE:
		//  MAIL
		//  RCPT
		// 	EHLO
		// 	HELO
		// 	RSET
		// 	NOOP
		// 	QUIT
		// 	VRFY

		switch verb {
		case "EHLO", "HELO":
			// reset to an initial state
			sess = &session{}

			if args == "" {
				args = "unknown"
			}

			sess.client = args
			sess.state = beforeMAIL
			writeReplyAndFlush(
				bw,
				250,
				fmt.Sprintf("%s greets %s", s.hostname, sess.client),
			)

		case "MAIL":
			if sess.state != beforeMAIL {
				respBadSequenceOfCommands(bw)
				continue
			}

			sess.tx = &transaction{}

			// TODO: handle Mail-parameters
			mailFrom := readMAILCommand(args)
			if mailFrom == "" {
				respInvalidSyntax(bw)
				continue
			}

			slog.Info("Received MAIL FROM", "mail_from", mailFrom)

			sess.tx.mailFrom = mailFrom
			sess.state = beforeRCPT

			respOK(bw)

		case "RCPT":
			if sess.state != beforeRCPT {
				respBadSequenceOfCommands(bw)
				continue
			}

			// TODO: handle Rcpt-parameters
			rcptTo := readRCPTCommand(args)
			if rcptTo == "" {
				respInvalidSyntax(bw)
				continue
			}

			// TODO: check the total number of recipients

			slog.Info("Received RCPT TO", "rcpt_to", rcptTo)

			sess.tx.rcptTo = append(sess.tx.rcptTo, rcptTo)
			sess.state = beforeDATA

			respOK(bw)

		case "DATA":
			if sess.state != beforeDATA {
				respBadSequenceOfCommands(bw)
				continue
			}

			writeReplyAndFlush(bw, 354, "Start mail input; end with <CRLF>.<CRLF>")
			sess.state = inDATA

			// limit to 30MB
			lr := io.LimitReader(br, 1024*1024*30)
			tr := textproto.NewReader(bufio.NewReader(lr))
			dr := tr.DotReader()

			data, err := io.ReadAll(dr)
			if err != nil {
				slog.Error("Failed to read DATA", "error", err.Error())
			}

			sess.tx.data = data

			fmt.Printf("=== BODY BEGIN ==\n%s=== BODY END ===\n", string(data))

			// TODO: processing
			respOK(bw)

			sess.state = afterDATA

		case "QUIT":
			quit = true
		case "NOOP":
			respOK(bw)

		case "RSET":
			sess.state = beforeMAIL
			sess.tx = &transaction{}
			respOK(bw)

		case "VRFY":
			writeReplyAndFlush(bw, 502, "Command not implemented")

		default:
			slog.Info("Unrecognized command received", "command", verb, "args", args)
			writeReplyAndFlush(bw, 500, "Syntax error")
		}
	}

	writeReplyAndFlush(bw, 221, "Service closing transmission channel")
}

func respInvalidSyntax(bw *bufio.Writer) {
	writeReplyAndFlush(bw, 501, "Syntax error in parameters or arguments")
}

func respBadSequenceOfCommands(bw *bufio.Writer) {
	writeReplyAndFlush(bw, 503, "Bad sequence of commands")
}

func respOK(bw *bufio.Writer) {
	writeReplyAndFlush(bw, 250, "OK")
}

func writeReplyAndFlush(bw *bufio.Writer, code int, reply ...string) {
	for i := 0; i < len(reply); i++ {
		if i+1 == len(reply) {
			fmt.Fprintf(bw, "%d %s\r\n", code, reply[i])
		} else {
			fmt.Fprintf(bw, "%d-%s\r\n", code, reply[i])
		}
	}

	if err := bw.Flush(); err != nil {
		slog.Info("Failed to flush the write", "error", err.Error())
	}
}

func readMAILCommand(args string) string {
	if len(args) < len("FROM:") {
		return ""
	}

	if strings.EqualFold("FROM:", args[:len("FROM:")]) {
		return strings.TrimSpace(args[len("FROM:"):])
	}

	return ""
}

func readRCPTCommand(args string) string {
	if len(args) < len("TO:") {
		return ""
	}

	if strings.EqualFold("TO:", args[:len("TO:")]) {
		return strings.TrimSpace(args[len("TO:"):])
	}

	return ""
}

func readCommand(br *bufio.Reader) (string, string, error) {
	l_, err := br.ReadString('\n')
	if err != nil {
		return "", "", err
	}

	l := strings.Trim(strings.Trim(l_, "\n"), "\r")

	cmd := strings.SplitN(l, " ", 2)

	if len(cmd) > 1 {
		return strings.ToUpper(cmd[0]), cmd[1], nil
	}

	return strings.ToUpper(cmd[0]), "", nil
}