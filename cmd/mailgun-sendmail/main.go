// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Mailgun-sendmail, a drop-in replacement for the standard
// Unix sendmail program, sends mail using Mailgun.
//
// Usage:
//
//	mailgun-sendmail [-itv] [-B type] [-b m] [-d val] [-F name] [-f addr] [-r addr] [addr ...]
//
// Mailgun-sendmail sends mail to the given addresses.
//
// The options are a subset of the standard sendmail options:
//
//	-i  ignore single dot lines on incoming message (default unless stdin is TTY)
//	-t  use To:, Cc:, Bcc: lines from input
//	-v  verbose mode
//
//	-B type
//	    set body type
//	-b code
//	    set mode code (must be "m", the default, meaning deliver a message from standard input)
//	-d val
//	    set debugging value
//	-F name
//	    set full name of sender
//	-f addr
//	    set address of sender
//	-r addr
//	    archaic equivalent of -f
//
// Configuration
//
// Mailgun-mail expects to find an mailgun API domain and authorization key
// of the form "<domain> api:key-<hexstring>" in the environment variable
// $MAILGUNKEY, or else in the file $HOME/.mailgun.key,
// or else in the file /etc/mailgun.key.
//
// Diagnostics
//
// If the file /var/log/mailgun.log can be opened for writing, mailgun
// logs its actions, successes, and failures there.
//
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/mail"
	"os"
	"sort"

	"rsc.io/getopt"
	"rsc.io/mailgun/cmd/internal/mg"
)

func usage() {
	mg.Logf("invalid command line")
	fmt.Fprintf(os.Stderr, "usage: mailgun-sendmail [options] [addr...]\n")
	getopt.PrintDefaults()
	os.Exit(2)
}

var (
	Bflag string
	bflag string
	dflag mg.StringListFlag
	Fflag string
	fflag string
	iflag bool
	tflag bool
	vflag bool

	to mg.AddrListFlag
)

func main() {
	mg.Init()

	flag.StringVar(&Bflag, "B", "", "set body `type` (ignored)")
	flag.StringVar(&bflag, "b", "m", "run operation named by `code` (must be m)")
	/*
		codes:
			ba ARPANET mode
			bd background daemon
			bD foreground daemon
			bh print persistent host status database
			bH purge persistent host status database
			bi initialize the alias database
			bm deliver mail in the usual way
			bp print a listing of the mail queue
			bs SMTP mode (SMTP on stdin/stdout)
			bt address test mode (for debugging config tables)
			bv verify names only (validating users or mailing lists)
	*/
	// flag.String("C", "", "use alternate config `file`")
	flag.Var(&dflag, "d", "set debugging `value` (http, nosend)")
	flag.StringVar(&Fflag, "F", "", "set the full `name` of the sender")
	flag.StringVar(&fflag, "f", "", "set the `from` address of the mail")
	flag.BoolVar(&iflag, "i", false, "ignore single dot lines on incoming message")
	flag.StringVar(&fflag, "r", "", "archaic alias for -f")
	flag.BoolVar(&tflag, "t", false, "read To:, Cc:, Bcc: lines from message")
	// flag.Bool("U", false, "ignored (initial user submission)")
	// flag.String("V", "", "set the envelope `id`")
	flag.BoolVar(&vflag, "v", false, "verbose mode")
	// flag.Var(&Oflag, "O", "", "set `option=value`")

	flag.Usage = usage
	getopt.Parse()
	for _, v := range dflag {
		switch v {
		default:
			mg.Die(fmt.Errorf("unknown debug value -d %s", v))
		case "http":
			mg.DebugHTTP = true
		case "nosend":
			mg.DisableMail = true
		}
	}
	mg.Verbose = vflag

	if bflag != "m" {
		mg.Die(fmt.Errorf("only sendmail -bm is supported"))
	}

	if flag.NArg() == 0 && !tflag {
		mg.Die(fmt.Errorf("no delivery addresses given"))
	}

	for _, arg := range flag.Args() {
		if err := to.Set(arg); err != nil {
			mg.Die(fmt.Errorf("cannot parse To: address: %v", err))
		}
	}

	// From address.
	from := new(mail.Address)
	from.Name = Fflag
	if fflag != "" {
		from.Address = fflag
	} else {
		from.Address = os.Getenv("USER")
		if from.Address == "" {
			mg.Die(fmt.Errorf("cannot determine From address: -f/-r not used, and $USER not set"))
		}
	}

	// Read message header from stdin.
	// At the least we need to delete the BCC line (apparently).
	// Note Header keys are as per textproto.CanonicalMIMEHeaderKey, so "Bcc" not "BCC".
	msg, err := mail.ReadMessage(os.Stdin)
	if err != nil {
		mg.Die(fmt.Errorf("reading message header: %v", err))
	}
	if tflag {
		for _, key := range []string{"To", "Cc", "Bcc"} {
			if len(msg.Header[key]) == 0 {
				continue
			}
			addrs, err := msg.Header.AddressList(key)
			if err != nil {
				mg.Die(fmt.Errorf("cannot parse %s: list: %v", key, err))
			}
			to = append(to, addrs...)
		}
		if len(to) == 0 {
			mg.Die(fmt.Errorf("no recipients found in message"))
		}
	}
	if len(msg.Header["From"]) == 0 {
		msg.Header["From"] = []string{from.String()}
	}
	delete(msg.Header, "Bcc")

	// TODO: Add Message-ID?

	var hdr bytes.Buffer
	var keys []string
	for k := range msg.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range msg.Header[k] {
			fmt.Fprintf(&hdr, "%s: %s\n", k, v)
		}
	}
	fmt.Fprintf(&hdr, "\n")

	body := msg.Body
	if mg.IsTTY && !iflag {
		body = &dotStopReader{r: body}
	}
	mime := io.MultiReader(&hdr, body)

	mg.MailMIME(from, to, mime)
}

type dotStopReader struct {
	r     io.Reader
	state int // how far into "\n.\n" have we seen?
	extra [3]byte
}

func (r *dotStopReader) Read(b []byte) (int, error) {
	if len(b) < 4 {
		panic("dotStopReader must read at least 4 bytes at a time")
	}
	if r.state == 3 {
		return 0, io.EOF
	}
	copy(b, r.extra[:r.state])
	n, err := r.r.Read(b[r.state:])
	n += r.state
	r.state = 0
Loop:
	for i, c := range b[:n] {
		switch r.state {
		case 0:
			if c == '\r' || c == '\n' {
				r.state = 1
			}
		case 1:
			if c == '.' {
				r.state = 2
			}
		case 2:
			if c == '\r' || c == '\n' {
				r.state = 3
				n = i + 1
				break Loop
			}
		}
	}
	if r.state > 0 {
		copy(r.extra[:], b[n-r.state:])
		n -= r.state
	}
	if n > 0 {
		return n, nil
	}
	return 0, err
}