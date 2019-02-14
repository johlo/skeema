package fs

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/alecthomas/participle"
	"github.com/alecthomas/participle/lexer"
	"github.com/skeema/tengo"
)

// StatementType indicates the type of a SQL statement found in a SQLFile.
// Parsing of types is very rudimentary, which can be advantageous for linting
// purposes. Otherwise, SQL errors or typos would prevent type detection.
type StatementType int

// Constants enumerating different types of statements
const (
	StatementTypeUnknown StatementType = iota
	StatementTypeNoop                  // entirely whitespace and/or comments
	StatementTypeUse
	StatementTypeCreate
	StatementTypeAlter
	// Other types will be added once they are supported by the package
)

// Statement represents a logical instruction in a file, consisting of either
// an SQL statement, a command (e.g. "USE some_database"), or whitespace and/or
// comments between two separate statements or commands.
type Statement struct {
	File            string
	LineNo          int
	CharNo          int
	Text            string
	DefaultDatabase string // only populated if a StatementTypeUse was encountered
	Type            StatementType
	ObjectType      tengo.ObjectType
	ObjectName      string
	ObjectQualifier string
	FromFile        *TokenizedSQLFile
}

// Location returns the file, line number, and character number where the
// statement was obtained from
func (stmt *Statement) Location() string {
	if stmt.File == "" && stmt.LineNo == 0 && stmt.CharNo == 0 {
		return ""
	}
	if stmt.File == "" {
		return fmt.Sprintf("unknown:%d:%d", stmt.LineNo, stmt.CharNo)
	}
	return fmt.Sprintf("%s:%d:%d", stmt.File, stmt.LineNo, stmt.CharNo)
}

// ObjectKey returns a tengo.ObjectKey for the object affected by this
// statement.
func (stmt *Statement) ObjectKey() tengo.ObjectKey {
	return tengo.ObjectKey{
		Type: stmt.ObjectType,
		Name: stmt.ObjectName,
	}
}

// Schema returns the schema name that this statement impacts.
func (stmt *Statement) Schema() string {
	if stmt.ObjectQualifier != "" {
		return stmt.ObjectQualifier
	}
	return stmt.DefaultDatabase
}

var reSplitTextBody = regexp.MustCompile(`(\s*;?\s*)$`)

// SplitTextBody returns Text with its trailing semicolon and whitespace (if
// any) separated out into a separate string.
func (stmt *Statement) SplitTextBody() (body string, suffix string) {
	// matches will always be a 2-elem slice, so no need to check for nil or len,
	// since all strings inherently match the regexp
	matches := reSplitTextBody.FindStringSubmatch(stmt.Text)
	return stmt.Text[:len(stmt.Text)-len(matches[1])], matches[1]
}

// Remove removes the statement from the list of statements in stmt.FromFile.
// It does not rewrite the file though.
func (stmt *Statement) Remove() {
	for i, comp := range stmt.FromFile.Statements {
		if stmt == comp {
			// from go wiki slicetricks -- delete slice element without leaking memory
			copy(stmt.FromFile.Statements[i:], stmt.FromFile.Statements[i+1:])
			stmt.FromFile.Statements[len(stmt.FromFile.Statements)-1] = nil
			stmt.FromFile.Statements = stmt.FromFile.Statements[:len(stmt.FromFile.Statements)-1]
			return
		}
	}
	panic(fmt.Errorf("Statement previously at %s not actually found in file", stmt.Location()))
}

type statementTokenizer struct {
	reader   *bufio.Reader
	filePath string // human-readable file path, just used for cosmetic purposes

	result []*Statement // completed statements
	stmt   *Statement   // tracking current (not yet completely tokenized) statement
	buf    bytes.Buffer // tracking text to eventually put into stmt

	lineNo          int    // human-readable line number, starting at 1
	inRelevant      bool   // true if current statement contains something other than just whitespace and comments
	inCComment      bool   // true if in a C-style comment
	inQuote         rune   // nonzero if inside of a quoted string; value indicates which quote rune
	defaultDatabase string // tracks most recent USE command
}

type lineState struct {
	*statementTokenizer
	line   string // current line of text, including trailing newline
	pos    int    // current byte offset within line
	charNo int    // human-readable column number, starting at 1
}

// newStatementTokenizer creates a tokenizer for splitting the contents of the
// reader into statements. The filePath is just used for cosmetic purposes.
func newStatementTokenizer(reader io.Reader, filePath string) *statementTokenizer {
	return &statementTokenizer{
		reader:   bufio.NewReader(reader),
		filePath: filePath,
	}
}

func (st *statementTokenizer) statements() ([]*Statement, error) {
	var err error
	for err != io.EOF {
		var line string
		line, err = st.reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return st.result, err
		}
		st.processLine(line, err == io.EOF)
	}
	if st.inQuote != 0 {
		err = fmt.Errorf("File %s has unterminated quote %c", st.filePath, st.inQuote)
	} else if st.inCComment {
		err = fmt.Errorf("File %s has unterminated C-style comment", st.filePath)
	} else {
		err = nil
	}
	return st.result, err
}

func (st *statementTokenizer) processLine(line string, eof bool) {
	st.lineNo++
	ls := &lineState{
		statementTokenizer: st,
		line:               line,
	}

	for ls.pos < len(ls.line) {
		c, cLen := ls.nextRune()
		if ls.stmt == nil {
			ls.beginStatement()
		}
		if ls.inCComment {
			if c == '*' && ls.peekRune() == '/' {
				ls.nextRune()
				ls.inCComment = false
			}
			continue
		} else if ls.inQuote > 0 {
			if c == '\\' && ls.peekRune() == ls.inQuote {
				ls.nextRune()
			} else if c == ls.inQuote {
				if ls.peekRune() == ls.inQuote {
					ls.nextRune()
				} else {
					ls.inQuote = 0
				}
			}
			continue
		}

		// C-style comment can be multi-line
		if c == '/' && ls.peekRune() == '*' {
			ls.inCComment = true
			ls.nextRune()
			continue
		}

		// Comment until end of line: Just put the rest of the line in the buffer
		// and move on to next line
		if c == '#' {
			ls.buf.WriteString(ls.line[ls.pos:])
			break
		}
		if c == '-' && ls.peekRune() == '-' {
			ls.nextRune()
			if unicode.IsSpace(ls.peekRune()) {
				ls.buf.WriteString(ls.line[ls.pos:])
				break
			}
		}

		// When transitioning from whitespace and/or comments, to something that
		// isn't whitespace or comments, split the whitespace/comments into its own
		// statement. That way, future file manipulations that change individual
		// statements won't remove any preceeding whitespace or comments.
		if !ls.inRelevant && !unicode.IsSpace(c) {
			ls.doneStatement(cLen)
			ls.inRelevant = true
		}

		switch c {
		case ';':
			if ls.peekRune() == '\n' {
				ls.nextRune()
			}
			ls.doneStatement(0)
		case '\n':
			// Commands do not require semicolons; newline alone can be delimiter.
			// Only supported command so far is USE.
			if strings.HasPrefix(strings.ToLower(ls.buf.String()), "use ") {
				ls.doneStatement(0)
			}
		case '"', '`', '\'':
			ls.inQuote = c
		}
	}

	// handle final statement before EOF, if anything left in buffer
	if eof {
		ls.doneStatement(0)
	}
}

// nextRune returns the rune at the current position, along with its length
// in bytes. It also advances to the next position.
func (ls *lineState) nextRune() (rune, int) {
	if ls.pos >= len(ls.line) {
		return 0, 0
	}
	c, cLen := utf8.DecodeRuneInString(ls.line[ls.pos:])
	ls.buf.WriteRune(c)
	ls.pos += cLen
	ls.charNo++
	return c, cLen
}

// peekRune returns the rune at the current position, without advancing.
func (ls *lineState) peekRune() rune {
	if ls.pos >= len(ls.line) {
		return 0
	}
	c, _ := utf8.DecodeRuneInString(ls.line[ls.pos:])
	return c
}

// beginStatement records the starting position of the next (not yet fully
// tokenized) statement.
func (ls *lineState) beginStatement() {
	ls.stmt = &Statement{
		File:            ls.filePath,
		LineNo:          ls.lineNo,
		CharNo:          ls.charNo,
		DefaultDatabase: ls.defaultDatabase,
	}
}

// doneStatement finalizes the current statement by filling in its text
// field with the buffer contents, optionally excluding the last omitEndBytes
// bytes of the buffer. It then puts this statement onto the result slice,
// and cleans up bookkeeping state in preparation for the next statement.
func (ls *lineState) doneStatement(omitEndBytes int) {
	bufLen := ls.buf.Len()
	if ls.stmt == nil || bufLen <= omitEndBytes {
		return
	}
	ls.stmt.Text = fmt.Sprintf("%s", ls.buf.Next(bufLen-omitEndBytes))
	ls.parseStatement()
	ls.result = append(ls.result, ls.stmt)
	ls.stmt = nil
	if omitEndBytes == 0 {
		ls.buf.Reset()
		ls.inRelevant = false
	} else {
		ls.beginStatement()
	}
}

func (ls *lineState) parseStatement() {
	txt := strings.TrimRight(ls.stmt.Text, ";\n\t ")
	if !ls.inRelevant || txt == "" {
		ls.stmt.Type = StatementTypeNoop
	} else {
		sqlStatement := &SQLStatement{}
		if err := nameParser.ParseString(txt, sqlStatement); err != nil {
			return
		} else if sqlStatement.Use != nil {
			ls.stmt.Type = StatementTypeUse
			ls.defaultDatabase = stripBackticks(sqlStatement.Use.DefaultDatabase)
		} else if sqlStatement.CreateTable != nil {
			ls.stmt.Type = StatementTypeCreate
			ls.stmt.ObjectType = tengo.ObjectTypeTable
			ls.stmt.ObjectQualifier, ls.stmt.ObjectName = sqlStatement.CreateTable.Name.SchemaAndTable()
		} else if sqlStatement.CreateProc != nil {
			ls.stmt.Type = StatementTypeCreate
			ls.stmt.ObjectType = tengo.ObjectTypeProc
			ls.stmt.ObjectQualifier, ls.stmt.ObjectName = sqlStatement.CreateProc.Name.SchemaAndTable()
		} else if sqlStatement.CreateFunc != nil {
			ls.stmt.Type = StatementTypeCreate
			ls.stmt.ObjectType = tengo.ObjectTypeFunc
			ls.stmt.ObjectQualifier, ls.stmt.ObjectName = sqlStatement.CreateFunc.Name.SchemaAndTable()
		}
	}
}

func stripBackticks(input string) string {
	if input[0] != '`' || input[len(input)-1] != '`' {
		return input
	}
	input = input[1 : len(input)-1]
	return strings.Replace(input, "``", "`", -1)
}

var (
	sqlLexer = lexer.Must(lexer.Regexp(`(#[^\n]+(?:\n|$))` +
		`|(--\s[^\n]+(?:\n|$))` +
		`|(/\*(.|\n)*?\*/)` +
		`|(\s+)` +
		"|(?P<Word>[0-9a-zA-Z$_]+|`(?:[^`]|``)+`)" +
		`|(?P<String>'(?:[^']|''|\')*'|"(?:[^"]|""|\")*")` +
		`|(?P<Number>[-+]?\d*\.?\d+([eE][-+]?\d+)?)` +
		`|(?P<Operator><>|!=|<=|>=|[-+*/%,.()=<>@])`,
	))
	nameParser = participle.MustBuild(&SQLStatement{},
		participle.Lexer(sqlLexer),
		participle.CaseInsensitive("Word"),
		participle.UseLookahead(10),
	)
)

// SQLStatement is the top-level struct for the name parser.
type SQLStatement struct {
	CreateTable *CreateTable `parser:"@@"`
	CreateProc  *CreateProc  `parser:"| @@"`
	CreateFunc  *CreateFunc  `parser:"| @@"`
	Use         *Use         `parser:"| @@"`
}

// ObjectName represents the name of an object, which may or may not be
// backtick-wrapped, and may or may not have multiple qualifier parts (each
// also potentially backtick-wrapped).
type ObjectName struct {
	Qualifiers []string `parser:"(@Word '.')*"`
	Name       string   `parser:"@Word"`
}

// SchemaAndTable interprets the ObjectName as a table name which may optionally
// have a schema name qualifier. The first return value is the schema name, or
// empty string if none was specified; the second return value is the table name.
func (n *ObjectName) SchemaAndTable() (string, string) {
	if len(n.Qualifiers) > 0 {
		return stripBackticks(n.Qualifiers[0]), stripBackticks(n.Name)
	}
	return "", stripBackticks(n.Name)
}

// Body slurps all body contents of a statement.
type Body struct {
	Contents string `parser:"(Word | String | Number | Operator)*"`
}

// Definer represents a user who is the definer of a routine or view.
type Definer struct {
	User string `parser:"((@String | @Word) '@'"`
	Host string `parser:"(@String | @Word))"`
	Func string `parser:"| ('CURRENT_USER' ('(' ')')?)"`
}

// CreateTable represents a CREATE TABLE statement.
type CreateTable struct {
	Name ObjectName `parser:"'CREATE' 'TABLE' ('IF' 'NOT' 'EXISTS')? @@"`
	Body Body       `parser:"@@"`
}

// CreateProc represents a CREATE PROCEDURE statement.
type CreateProc struct {
	Definer *Definer   `parser:"'CREATE' ('DEFINER' '=' @@)?"`
	Name    ObjectName `parser:"'PROCEDURE' @@"`
	Body    Body       `parser:"@@"`
}

// CreateFunc represents a CREATE FUNCTION statement.
type CreateFunc struct {
	Definer *Definer   `parser:"'CREATE' ('DEFINER' '=' @@)?"`
	Name    ObjectName `parser:"'FUNCTION' @@"`
	Body    Body       `parser:"@@"`
}

// Use represents a USE command.
type Use struct {
	DefaultDatabase string `parser:"'USE' @Word"`
}
