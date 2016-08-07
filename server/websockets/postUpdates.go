package websockets

import (
	"errors"

	"github.com/bakape/meguca/db"
	"github.com/bakape/meguca/parser"
	"github.com/bakape/meguca/types"
	"github.com/bakape/meguca/util"
	r "github.com/dancannon/gorethink"
)

var (
	errNoPostOpen = errors.New("no post open")
	errLineEmpty  = errors.New("line empty")
)

// Shorthand. We use it a lot for update query construction.
type msi map[string]interface{}

// Append a rune to the body of the open post
func appendRune(data []byte, c *Client) error {
	if !c.hasPost() {
		return errNoPostOpen
	}
	if c.openPost.bodyLength+1 > parser.MaxLengthBody {
		return parser.ErrBodyTooLong
	}
	var char rune
	if err := decodeMessage(data, &char); err != nil {
		return err
	}

	if char == '\n' {
		return parseLine(c)
	}

	id := c.openPost.id
	msg, err := encodeMessage(messageAppend, [2]int64{id, int64(char)})
	if err != nil {
		return err
	}

	update := msi{
		"body": postBody(id).Add(string(char)),
	}
	if err := c.updatePost(update, msg); err != nil {
		return err
	}
	c.openPost.WriteRune(char)
	c.openPost.bodyLength++

	return nil
}

// Shorthand for retrievinf the post's body field from a thread document
func postBody(id int64) r.Term {
	return r.Row.
		Field("posts").
		Field(util.IDToString(id)).
		Field("body")
}

// Helper for running post update queries on the current open post
func (c *Client) updatePost(update msi, msg []byte) error {
	q := r.
		Table("threads").
		Get(c.openPost.op).
		Update(createUpdate(c.openPost.id, update, msg))
	return db.Write(q)
}

// Helper for creating post update maps
func createUpdate(id int64, update msi, msg []byte) msi {
	return msi{
		"log": appendLog(msg),
		"posts": msi{
			util.IDToString(id): update,
		},
	}
}

// Shorthand for creating a replication log append query
func appendLog(msg []byte) r.Term {
	return r.Row.Field("log").Append(msg)
}

// Parse line contents and commit newline. If line contains hash commands or
// links to other posts also commit those and generate backlinks, if needed.
func parseLine(c *Client) error {
	c.openPost.bodyLength++
	links, comm, err := parser.ParseLine(c.openPost.Bytes(), c.openPost.board)
	if err != nil {
		return err
	}
	defer c.openPost.Reset()

	msg, err := encodeMessage(messageAppend, [2]int64{
		c.openPost.id,
		int64('\n'),
	})
	if err != nil {
		return err
	}
	idStr := util.IDToString(c.openPost.id)
	update := msi{
		"body": r.Row.
			Field("posts").
			Field(idStr).
			Field("body").
			Add("\n"),
	}
	if err := c.updatePost(update, msg); err != nil {
		return err
	}

	switch {
	case comm.Val != nil:
		return writeCommand(comm, idStr, c)
	case links != nil:
		return writeLinks(links, c)
	default:
		return nil
	}
}

// Write a hash command to the database
func writeCommand(comm types.Command, idStr string, c *Client) error {
	msg, err := encodeMessage(messageCommand, comm)
	if err != nil {
		return err
	}
	update := msi{
		"commands": r.Row.
			Field("posts").
			Field(idStr).
			Field("commands").
			Default([]types.Command{}).
			Append(comm),
	}
	return c.updatePost(update, msg)
}

// Write new links to other posts to the database
func writeLinks(links types.LinkMap, c *Client) error {
	msg, err := encodeMessage(messageLink, links)
	if err != nil {
		return err
	}
	update := msi{
		"links": links,
	}
	if err := c.updatePost(update, msg); err != nil {
		return err
	}

	// Most often this loop will iterate only once, so no need to think heavily
	// about optimisations
	for destID := range links {
		id := c.openPost.id
		op := c.openPost.op
		board := c.openPost.board
		if err := writeBacklink(id, op, board, destID); err != nil {
			return err
		}
	}

	return nil
}

// Writes the location data of the post linking a post to the the post being
// linked
func writeBacklink(id, op int64, board string, destID int64) error {
	msg, err := encodeMessage(messageBacklink, types.LinkMap{
		id: {
			OP:    op,
			Board: board,
		},
	})
	if err != nil {
		return err
	}

	update := msi{
		"backlinks": msi{
			util.IDToString(id): types.Link{
				OP:    op,
				Board: board,
			},
		},
	}
	q := r.
		Table("threads").
		GetAllByIndex("post", destID).
		Update(createUpdate(destID, update, msg))

	return db.Write(q)
}

// Remove one character from the end of the line in the open post
func backspace(_ []byte, c *Client) error {
	if !c.hasPost() {
		return errNoPostOpen
	}
	length := c.openPost.Len()
	if length == 0 {
		return errLineEmpty
	}
	c.openPost.Truncate(length - 1)
	c.openPost.bodyLength--

	id := c.openPost.id
	update := msi{
		"body": postBody(id).Slice(0, -1),
	}
	msg, err := encodeMessage(messageBackspace, id)
	if err != nil {
		return err
	}
	return c.updatePost(update, msg)
}
