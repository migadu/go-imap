package imapclient

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func getSelectOpts(options *imap.ListOptions) []string {
	if options == nil {
		return nil
	}

	var l []string
	if options.SelectSubscribed {
		l = append(l, "SUBSCRIBED")
	}
	if options.SelectRemote {
		l = append(l, "REMOTE")
	}
	if options.SelectRecursiveMatch {
		l = append(l, "RECURSIVEMATCH")
	}
	if options.SelectSpecialUse {
		l = append(l, "SPECIAL-USE")
	}
	return l
}

func getReturnOpts(options *imap.ListOptions) []string {
	if options == nil {
		return nil
	}

	var l []string
	if options.ReturnSubscribed {
		l = append(l, "SUBSCRIBED")
	}
	if options.ReturnChildren {
		l = append(l, "CHILDREN")
	}
	if options.ReturnStatus != nil {
		l = append(l, "STATUS")
	}
	if options.ReturnSpecialUse {
		l = append(l, "SPECIAL-USE")
	}
	if options.ReturnMetadata != nil {
		l = append(l, "METADATA")
	}
	return l
}

// List sends a LIST command.
//
// The caller must fully consume the ListCommand. A simple way to do so is to
// defer a call to ListCommand.Close.
//
// A nil options pointer is equivalent to a zero options value.
//
// A non-zero options value requires support for IMAP4rev2 or the LIST-EXTENDED
// extension.
func (c *Client) List(ref, pattern string, options *imap.ListOptions) *ListCommand {
	cmd := &ListCommand{
		mailboxes:      make(chan *imap.ListData, 64),
		ref:            ref,
		pattern:        pattern,
		returnStatus:   options != nil && options.ReturnStatus != nil,
		returnMetadata: options != nil && options.ReturnMetadata != nil,
	}
	enc := c.beginCommand("LIST", cmd)
	if selectOpts := getSelectOpts(options); len(selectOpts) > 0 {
		enc.SP().List(len(selectOpts), func(i int) {
			enc.Atom(selectOpts[i])
		})
	}
	enc.SP().Mailbox(ref).SP().Mailbox(pattern)
	if returnOpts := getReturnOpts(options); len(returnOpts) > 0 {
		enc.SP().Atom("RETURN").SP().List(len(returnOpts), func(i int) {
			opt := returnOpts[i]
			enc.Atom(opt)
			switch opt {
			case "STATUS":
				returnStatus := statusItems(options.ReturnStatus)
				enc.SP().List(len(returnStatus), func(j int) {
					enc.Atom(returnStatus[j])
				})
			case "METADATA":
				if len(options.ReturnMetadata) > 0 {
					enc.SP().List(len(options.ReturnMetadata), func(j int) {
						enc.String(options.ReturnMetadata[j])
					})
				}
			}
		})
	}
	enc.end()
	return cmd
}

// Lsub sends an LSUB command (RFC 3501 section 6.3.9), which lists the
// subscribed mailboxes.
//
// IMAP4rev2 removes LSUB in favor of LIST (SUBSCRIBED), which needs IMAP4rev2
// or LIST-EXTENDED. LSUB is how an IMAP4rev1 server without LIST-EXTENDED
// tells which mailboxes are subscribed. Its answer is LIST data, with one
// difference: a mailbox that is not subscribed may be listed with
// \Noselect, when the pattern ends in "%" and the mailbox has subscribed
// children.
//
// The caller must fully consume the ListCommand, as for List.
func (c *Client) Lsub(ref, pattern string) *ListCommand {
	cmd := &ListCommand{
		mailboxes: make(chan *imap.ListData, 64),
		ref:       ref,
		pattern:   pattern,
		lsub:      true,
	}
	enc := c.beginCommand("LSUB", cmd)
	enc.SP().Mailbox(ref).SP().Mailbox(pattern)
	enc.end()
	return cmd
}

func (c *Client) handleList() error {
	data, err := readList(c.dec)
	if err != nil {
		return fmt.Errorf("in LIST: %w", err)
	}

	cmd := c.findPendingCmdFunc(func(cmd command) bool {
		switch cmd := cmd.(type) {
		case *ListCommand:
			// A LIST response is never an answer to LSUB: one that arrives
			// while LSUB is in flight is unsolicited (NOTIFY) and says
			// nothing about subscriptions.
			return !cmd.lsub && cmd.matches(data)
		case *SelectCommand:
			return cmd.mailbox == data.Mailbox && cmd.data.List == nil
		default:
			return false
		}
	})
	switch cmd := cmd.(type) {
	case *ListCommand:
		if cmd.returnStatus || cmd.returnMetadata {
			if cmd.pendingData != nil {
				cmd.mailboxes <- cmd.pendingData
			}
			cmd.pendingData = data
		} else {
			cmd.mailboxes <- data
		}
	case *SelectCommand:
		cmd.data.List = data
	}

	if cmd == nil {
		if handler := c.options.unilateralDataHandler().List; handler != nil {
			handler(data)
		}
	}

	return nil
}

// handleLsub reads an LSUB response, which has the grammar of a LIST
// response, and hands it to the LSUB command it answers. There is no
// unsolicited LSUB response: one that answers no LSUB command in flight is
// dropped.
func (c *Client) handleLsub() error {
	data, err := readList(c.dec)
	if err != nil {
		return fmt.Errorf("in LSUB: %w", err)
	}

	isLsub := func(cmd command) bool {
		list, ok := cmd.(*ListCommand)
		return ok && list.lsub
	}
	// The pattern only picks between LSUB commands in flight. A response that
	// matches none still answers one of them, since none is unsolicited, and
	// the server's reading of the reference may be one matches cannot follow.
	cmd := c.findPendingCmdFunc(func(cmd command) bool {
		return isLsub(cmd) && cmd.(*ListCommand).matches(data)
	})
	if cmd == nil {
		cmd = c.findPendingCmdFunc(isLsub)
	}
	if cmd != nil {
		cmd.(*ListCommand).mailboxes <- data
	}
	return nil
}

// ListCommand is a LIST or an LSUB command.
type ListCommand struct {
	commandBase
	mailboxes chan *imap.ListData

	ref            string
	pattern        string
	lsub           bool // an LSUB command, answered with LSUB responses
	returnStatus   bool
	returnMetadata bool
	pendingData    *imap.ListData
}

// matches reports whether a LIST response belongs to this command rather than
// being unsolicited. With NOTIFY (RFC 5465 §5.4, §5.5) a LIST response can
// arrive at any time, including while a LIST command is in flight.
//
// The test is deliberately lopsided: mistaking the command's own response for
// an unsolicited one loses data the caller asked for, while the reverse merely
// adds a mailbox to the results. So anything that could plausibly answer this
// command is claimed, and only a clear mismatch is treated as unsolicited.
func (cmd *ListCommand) matches(data *imap.ListData) bool {
	if data.ChildInfo != nil {
		// A RECURSIVEMATCH parent is reported because of its children, so it
		// need not match the pattern itself.
		return true
	}
	if cmd.pattern == "" {
		// RFC 9051 §6.3.9: an empty mailbox name asks for the hierarchy
		// delimiter, answered with the reference's root — which by definition
		// does not match the (empty) pattern.
		return true
	}
	// The wire form is what the server matched against: INBOX is canonicalized
	// to upper case in both directions (RFC 9051 §6.3.9 has the server match
	// and report the uppercase spelling), so compare the canonical forms.
	ref, pattern := canonicalMailbox(cmd.ref), canonicalMailbox(cmd.pattern)
	if internal.MatchList(data.Mailbox, data.Delim, ref, pattern) {
		return true
	}
	if ref != "" && data.Delim != 0 {
		delim := string(data.Delim)
		if !strings.HasSuffix(ref, delim) {
			// RFC 9051 §6.3.9 gives three interpretations of "foo/bar" +
			// "rag/baz": the child path above, direct concatenation, or a
			// path relative to "foo/". Accept the latter two as well. Strip
			// the reference literally: its '*' and '%' are not wildcards.
			prefixes := []string{ref}
			if i := strings.LastIndex(ref, delim); i >= 0 {
				prefixes = append(prefixes, ref[:i+len(delim)])
			}
			for _, prefix := range prefixes {
				if suffix, ok := strings.CutPrefix(data.Mailbox, prefix); ok &&
					internal.MatchPattern(suffix, data.Delim, pattern) {
					return true
				}
			}
		}
	}
	// A wildcard-leading pattern stays in the reference's hierarchy, whatever
	// the reference's form or the delimiter. Matching it against the whole
	// name would absorb unrelated LIST notifications (e.g. INBOX while listing
	// "Archive" "*").
	if ref != "" && (pattern[0] == '*' || pattern[0] == '%') {
		return false
	}
	// Breakout characters are server-defined, so literal-leading patterns
	// may override the reference. Do not restrict this to known /, ~ or #
	// prefixes, or attempt to reproduce the server's full canonicalization.
	return internal.MatchPattern(data.Mailbox, data.Delim, pattern)
}

// canonicalMailbox spells a mailbox name the way the encoder puts it on the
// wire, so that a name coming back from the server can be compared with it.
//
// It mirrors Encoder.Mailbox exactly: only a name that is entirely "INBOX" is
// rewritten. Anything else is sent verbatim, so rewriting a mere prefix here
// would compare against a string the server never saw — and drop the response.
func canonicalMailbox(name string) string {
	const inbox = "INBOX"
	if strings.EqualFold(name, inbox) {
		return inbox
	}
	return name
}

// Next advances to the next mailbox.
//
// On success, the mailbox LIST data is returned. On error or if there are no
// more mailboxes, nil is returned.
func (cmd *ListCommand) Next() *imap.ListData {
	return <-cmd.mailboxes
}

// Close releases the command.
//
// Calling Close unblocks the IMAP client decoder and lets it read the next
// responses. Next will always return nil after Close.
func (cmd *ListCommand) Close() error {
	for cmd.Next() != nil {
		// ignore
	}
	return cmd.wait()
}

// Collect accumulates mailboxes into a list.
//
// This is equivalent to calling Next repeatedly and then Close.
func (cmd *ListCommand) Collect() ([]*imap.ListData, error) {
	var l []*imap.ListData
	for {
		data := cmd.Next()
		if data == nil {
			break
		}
		l = append(l, data)
	}
	return l, cmd.Close()
}

func readList(dec *imapwire.Decoder) (*imap.ListData, error) {
	var data imap.ListData

	var err error
	data.Attrs, err = internal.ExpectMailboxAttrList(dec)
	if err != nil {
		return nil, fmt.Errorf("in mbx-list-flags: %w", err)
	}

	if !dec.ExpectSP() {
		return nil, dec.Err()
	}

	data.Delim, err = readDelim(dec)
	if err != nil {
		return nil, err
	}

	if !dec.ExpectSP() || !dec.ExpectMailbox(&data.Mailbox) {
		return nil, dec.Err()
	}

	if dec.SP() {
		err := dec.ExpectList(func() error {
			var tag string
			if !dec.ExpectAString(&tag) || !dec.ExpectSP() {
				return dec.Err()
			}
			var err error
			switch strings.ToUpper(tag) {
			case "CHILDINFO":
				data.ChildInfo, err = readChildInfoExtendedItem(dec)
				if err != nil {
					return fmt.Errorf("in childinfo-extended-item: %w", err)
				}
			case "OLDNAME":
				data.OldName, err = readOldNameExtendedItem(dec)
				if err != nil {
					return fmt.Errorf("in oldname-extended-item: %w", err)
				}
			default:
				if !dec.DiscardValue() {
					return fmt.Errorf("in tagged-ext-val: %w", err)
				}
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("in mbox-list-extended: %w", err)
		}
	}

	return &data, nil
}

func readChildInfoExtendedItem(dec *imapwire.Decoder) (*imap.ListDataChildInfo, error) {
	var childInfo imap.ListDataChildInfo
	err := dec.ExpectList(func() error {
		var opt string
		if !dec.ExpectAString(&opt) {
			return dec.Err()
		}
		if strings.ToUpper(opt) == "SUBSCRIBED" {
			childInfo.Subscribed = true
		}
		return nil
	})
	return &childInfo, err
}

func readOldNameExtendedItem(dec *imapwire.Decoder) (string, error) {
	var name string
	if !dec.ExpectSpecial('(') || !dec.ExpectMailbox(&name) || !dec.ExpectSpecial(')') {
		return "", dec.Err()
	}
	return name, nil
}

func readDelim(dec *imapwire.Decoder) (rune, error) {
	var delimStr string
	if dec.Quoted(&delimStr) {
		delim, size := utf8.DecodeRuneInString(delimStr)
		if delim == utf8.RuneError || size != len(delimStr) {
			return 0, fmt.Errorf("mailbox delimiter must be a single rune")
		}
		return delim, nil
	} else if !dec.ExpectNIL() {
		return 0, dec.Err()
	} else {
		return 0, nil
	}
}
