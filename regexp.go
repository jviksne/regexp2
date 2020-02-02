/*
Package regexp2 is a regexp package that has an interface similar to Go's framework regexp engine but uses a
more feature full regex engine behind the scenes.

It doesn't have constant time guarantees, but it allows backtracking and is compatible with Perl5 and .NET.
You'll likely be better off with the RE2 engine from the regexp package and should only use this if you
need to write very complex patterns or require compatibility with .NET.
*/
package regexp2

import (
	"errors"
	"math"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jviksne/regexp2/syntax"
)

// Default timeout used when running regexp matches -- "forever"
var DefaultMatchTimeout = time.Duration(math.MaxInt64)

// Regexp is the representation of a compiled regular expression.
// A Regexp is safe for concurrent use by multiple goroutines.
type Regexp struct {
	//timeout when trying to find matches
	MatchTimeout time.Duration

	// read-only after Compile
	pattern string       // as passed to Compile
	options RegexOptions // options

	caps     map[int]int    // capnum->index
	capnames map[string]int //capture group name -> index
	capslist []string       //sorted list of capture group names
	capsize  int            // size of the capture array

	code *syntax.Code // compiled program

	// cache of machines for running regexp
	muRun  sync.Mutex
	runner []*runner
}

// Compile parses a regular expression and returns, if successful,
// a Regexp object that can be used to match against text.
func Compile(expr string, opt RegexOptions) (*Regexp, error) {
	// parse it
	tree, err := syntax.Parse(expr, syntax.RegexOptions(opt))
	if err != nil {
		return nil, err
	}

	// translate it to code
	code, err := syntax.Write(tree)
	if err != nil {
		return nil, err
	}

	// return it
	return &Regexp{
		pattern:      expr,
		options:      opt,
		caps:         code.Caps,
		capnames:     tree.Capnames,
		capslist:     tree.Caplist,
		capsize:      code.Capsize,
		code:         code,
		MatchTimeout: DefaultMatchTimeout,
	}, nil
}

// MustCompile is like Compile but panics if the expression cannot be parsed.
// It simplifies safe initialization of global variables holding compiled regular
// expressions.
func MustCompile(str string, opt RegexOptions) *Regexp {
	regexp, error := Compile(str, opt)
	if error != nil {
		panic(`regexp2: Compile(` + quote(str) + `): ` + error.Error())
	}
	return regexp
}

// Escape adds backslashes to any special characters in the input string
func Escape(input string) string {
	return syntax.Escape(input)
}

// Unescape removes any backslashes from previously-escaped special characters in the input string
func Unescape(input string) (string, error) {
	return syntax.Unescape(input)
}

// String returns the source text used to compile the regular expression.
func (re *Regexp) String() string {
	return re.pattern
}

func quote(s string) string {
	if strconv.CanBackquote(s) {
		return "`" + s + "`"
	}
	return strconv.Quote(s)
}

// RegexOptions impact the runtime and parsing behavior
// for each specific regex.  They are setable in code as well
// as in the regex pattern itself.
type RegexOptions int32

const (
	None                    RegexOptions = 0x0
	IgnoreCase                           = 0x0001 // "i"
	Multiline                            = 0x0002 // "m"
	ExplicitCapture                      = 0x0004 // "n"
	Compiled                             = 0x0008 // "c"
	Singleline                           = 0x0010 // "s"
	IgnorePatternWhitespace              = 0x0020 // "x"
	RightToLeft                          = 0x0040 // "r"
	Debug                                = 0x0080 // "d"
	ECMAScript                           = 0x0100 // "e"
	RE2                                  = 0x0200 // RE2 (regexp package) compatibility mode
)

func (re *Regexp) RightToLeft() bool {
	return re.options&RightToLeft != 0
}

func (re *Regexp) Debug() bool {
	return re.options&Debug != 0
}

// Replace searches the input string and replaces each match found with the replacement text.
// Count will limit the number of matches attempted and startAt will allow
// us to skip past possible matches at the start of the input (left or right depending on RightToLeft option).
// Set startAt and count to -1 to go through the whole string
func (re *Regexp) Replace(input, replacement string, startAt, count int) (string, error) {
	data, err := syntax.NewReplacerData(replacement, re.caps, re.capsize, re.capnames, syntax.RegexOptions(re.options))
	if err != nil {
		return "", err
	}
	//TODO: cache ReplacerData

	return replace(re, data, nil, input, startAt, count)
}

// ReplaceFunc searches the input string and replaces each match found using the string from the evaluator
// Count will limit the number of matches attempted and startAt will allow
// us to skip past possible matches at the start of the input (left or right depending on RightToLeft option).
// Set startAt and count to -1 to go through the whole string.
func (re *Regexp) ReplaceFunc(input string, evaluator MatchEvaluator, startAt, count int) (string, error) {
	return replace(re, nil, evaluator, input, startAt, count)
}

// FindStringMatch searches the input string for a Regexp match
func (re *Regexp) FindStringMatch(s string) (*Match, error) {
	// convert string to runes
	return re.run(false, -1, getRunes(s))
}

// FindRunesMatch searches the input rune slice for a Regexp match
func (re *Regexp) FindRunesMatch(r []rune) (*Match, error) {
	return re.run(false, -1, r)
}

// FindStringMatchStartingAt searches the input string for a Regexp match starting at the startAt index
func (re *Regexp) FindStringMatchStartingAt(s string, startAt int) (*Match, error) {
	if startAt > len(s) {
		return nil, errors.New("startAt must be less than the length of the input string")
	}
	r, startAt := re.getRunesAndStart(s, startAt)
	if startAt == -1 {
		// we didn't find our start index in the string -- that's a problem
		return nil, errors.New("startAt must align to the start of a valid rune in the input string")
	}

	return re.run(false, startAt, r)
}

// FindRunesMatchStartingAt searches the input rune slice for a Regexp match starting at the startAt index
func (re *Regexp) FindRunesMatchStartingAt(r []rune, startAt int) (*Match, error) {
	return re.run(false, startAt, r)
}

// FindNextMatch returns the next match in the same input string as the match parameter.
// Will return nil if there is no next match or if given a nil match.
func (re *Regexp) FindNextMatch(m *Match) (*Match, error) {
	if m == nil {
		return nil, nil
	}

	// If previous match was empty, advance by one before matching to prevent
	// infinite loop
	startAt := m.textpos
	if m.Length == 0 {
		if m.textpos == len(m.text) {
			return nil, nil
		}

		if re.RightToLeft() {
			startAt--
		} else {
			startAt++
		}
	}
	return re.run(false, startAt, m.text)
}

// MatchString return true if the string matches the regex
// error will be set if a timeout occurs
func (re *Regexp) MatchString(s string) (bool, error) {
	m, err := re.run(true, -1, getRunes(s))
	if err != nil {
		return false, err
	}
	return m != nil, nil
}

func (re *Regexp) getRunesAndStart(s string, startAt int) ([]rune, int) {
	if startAt < 0 {
		if re.RightToLeft() {
			r := getRunes(s)
			return r, len(r)
		}
		return getRunes(s), 0
	}
	ret := make([]rune, len(s))
	i := 0
	runeIdx := -1
	for strIdx, r := range s {
		if strIdx == startAt {
			runeIdx = i
		}
		ret[i] = r
		i++
	}
	return ret[:i], runeIdx
}

func getRunes(s string) []rune {
	ret := make([]rune, len(s))
	i := 0
	for _, r := range s {
		ret[i] = r
		i++
	}
	return ret[:i]
}

// MatchRunes return true if the runes matches the regex
// error will be set if a timeout occurs
func (re *Regexp) MatchRunes(r []rune) (bool, error) {
	m, err := re.run(true, -1, r)
	if err != nil {
		return false, err
	}
	return m != nil, nil
}

// GetGroupNames Returns the set of strings used to name capturing groups in the expression.
func (re *Regexp) GetGroupNames() []string {
	var result []string

	if re.capslist == nil {
		result = make([]string, re.capsize)

		for i := 0; i < len(result); i++ {
			result[i] = strconv.Itoa(i)
		}
	} else {
		result = make([]string, len(re.capslist))
		copy(result, re.capslist)
	}

	return result
}

// GetGroupNumbers returns the integer group numbers corresponding to a group name.
func (re *Regexp) GetGroupNumbers() []int {
	var result []int

	if re.caps == nil {
		result = make([]int, re.capsize)

		for i := 0; i < len(result); i++ {
			result[i] = i
		}
	} else {
		result = make([]int, len(re.caps))

		for k, v := range re.caps {
			result[v] = k
		}
	}

	return result
}

// GroupNameFromNumber retrieves a group name that corresponds to a group number.
// It will return "" for and unknown group number.  Unnamed groups automatically
// receive a name that is the decimal string equivalent of its number.
func (re *Regexp) GroupNameFromNumber(i int) string {
	if re.capslist == nil {
		if i >= 0 && i < re.capsize {
			return strconv.Itoa(i)
		}

		return ""
	}

	if re.caps != nil {
		var ok bool
		if i, ok = re.caps[i]; !ok {
			return ""
		}
	}

	if i >= 0 && i < len(re.capslist) {
		return re.capslist[i]
	}

	return ""
}

// GroupNumberFromName returns a group number that corresponds to a group name.
// Returns -1 if the name is not a recognized group name.  Numbered groups
// automatically get a group name that is the decimal string equivalent of its number.
func (re *Regexp) GroupNumberFromName(name string) int {
	// look up name if we have a hashtable of names
	if re.capnames != nil {
		if k, ok := re.capnames[name]; ok {
			return k
		}

		return -1
	}

	// convert to an int if it looks like a number
	result := 0
	for i := 0; i < len(name); i++ {
		ch := name[i]

		if ch > '9' || ch < '0' {
			return -1
		}

		result *= 10
		result += int(ch - '0')
	}

	// return int if it's in range
	if result >= 0 && result < re.capsize {
		return result
	}

	return -1
}

// FindAllStringIndex is the 'All' version of FindStringIndex; it returns a
// slice of all successive matches of the expression.
// A return value of nil indicates no match.
//
// Ported from https://golang.org/src/regexp/regexp.go
//
// Regexp equivalent:
// r, _ := regexp.Compile("p([a-z]+)ch")
// fmt.Println(r.FindAllStringIndex("peach punch", -1))
//
// [[0 5] [6 11]]
// 
func (re *Regexp) FindAllStringIndex(s string, n int) [][]int {

	var result [][]int

	if n < 0 {
		n = len(s) + 1
	}	

	c := 0

	m, _ := re.FindStringMatch(s);

	// Loop through all matches and append pairs of full match indexes
	for m != nil && c < n {

		c = c + 1

		groups := m.Groups()

		if len(groups) > 0 {
			result = append(result, []int{groups[0].Index, groups[0].Index + groups[0].Length})
		}

		m, _ = re.FindNextMatch(m)

	}

	return result

}

// FindStringIndex returns a two-element slice of integers defining the
// location of the leftmost match in s of the regular expression. The match
// itself is at s[loc[0]:loc[1]].
// A return value of nil indicates no match.
//
// Ported from https://golang.org/src/regexp/regexp.go
//
// Regexp equivalent:
// r, _ := regexp.Compile("p([a-z]+)ch")
// fmt.Println(r.FindStringSubmatchIndex("peach punch"))
//
// [0 5]
func (re *Regexp) FindStringIndex(s string) (loc []int) {

	if m, _ := re.FindStringMatch(s); m != nil {

		groups := m.Groups()

		if len(groups) > 0 {
			return []int{groups[0].Index, groups[0].Index + groups[0].Length}
		}

	}

	return nil
}

// FindStringSubmatchIndex returns a slice holding the index pairs
// identifying the leftmost match of the regular expression in s and the
// matches, if any, of its subexpressions.
// A return value of nil indicates no match.
//
// Ported from https://golang.org/src/regexp/regexp.go
//
// Regexp equivalent:
// r, _ := regexp.Compile("p([a-z]+)ch")
//
// [0 5 1 3]
func (re *Regexp) FindStringSubmatchIndex(s string) []int {

	var result []int

	if m, _ := re.FindStringMatch(s); m != nil {

		for _, g := range m.Groups() {

			result = append(result, g.Index)

			result = append(result, g.Index + g.Length)
		}

	}

	return result
}

// FindAllStringSubmatchIndex is the 'All' version of
// FindStringSubmatchIndex; it returns a slice of all successive matches of
// the expression.
// A return value of nil indicates no match.
//
// Ported from https://golang.org/src/regexp/regexp.go
//
// Regexp equivalent:
// r, _ := regexp.Compile("p([a-z]+)ch")
// fmt.Println(r.FindAllStringSubmatchIndex("peach punch pinch", -1))
//
// [[0 5 1 3] [6 11 7 9] [12 17 13 15]]
// 
func (re *Regexp) FindAllStringSubmatchIndex(s string, n int) [][]int {

	var result [][]int

	if n < 0 {
		n = len(s) + 1
	}

	m, _ := re.FindStringMatch(s);

	c := 0

	// Loop through all matches and append pairs of full match indexes
	for m != nil && c < n {

		c = c + 1

		var subres []int

		for _, g := range m.Groups() {

			subres = append(subres, g.Index)

			subres = append(subres, g.Index + g.Length)
		}

		result = append(result, subres)

		m, _ = re.FindNextMatch(m)

	}

	return result

}

// FindAllSubmatchIndex is the 'All' version of FindSubmatchIndex; it returns
// a slice of all successive matches of the expression, as defined by the
// 'All' description in the package comment.
// A return value of nil indicates no match.
//
// Ported from https://golang.org/src/regexp/regexp.go
//
func (re *Regexp) FindAllSubmatchIndex(b []byte, n int) [][]int {
	return re.FindAllStringSubmatchIndex(string(b), n)
}

// ReplaceAllFunc returns a copy of src in which all matches of the
// Regexp have been replaced by the return value of function repl applied
// to the matched byte slice. The replacement returned by repl is substituted
// directly, without using Expand.
//
// Ported from https://golang.org/src/regexp/regexp.go
//
func (re *Regexp) ReplaceAllFunc(src []byte, repl func([]byte) []byte) []byte {
	return re.replaceAll(string(src), func(dst []byte, match []int) []byte {
		return append(dst, repl(src[match[0]:match[1]])...)
	})
}

//
// Ported from https://golang.org/src/regexp/regexp.go
//
// TODO: verify that UTF8 can safely be used here
//
func (re *Regexp) replaceAll(src string, repl func(dst []byte, m []int) []byte) []byte {
	lastMatchEnd := 0 // end position of the most recent match
	searchPos := 0    // position where we next look for a match
	var buf []byte

	m, _ := re.FindStringMatch(src);
	
	for m != nil {

		a := []int{m.Group.Index, m.Group.Index + m.Group.Length};

		// Copy the unmatched characters before this match.
		buf = append(buf, src[lastMatchEnd:a[0]]...)

		// Now insert a copy of the replacement string, but not for a
		// match of the empty string immediately after another match.
		// (Otherwise, we get double replacement for patterns that
		// match both empty and nonempty strings.)
		if a[1] > lastMatchEnd || a[0] == 0 {
			buf = repl(buf, a)
		}
		lastMatchEnd = a[1]

		// Advance past this match; always advance at least one character.
		var width int
		_, width = utf8.DecodeRuneInString(src[searchPos:])
		if searchPos+width > a[1] {
			searchPos += width
		} else if searchPos+1 > a[1] {
			// This clause is only needed at the end of the input
			// string. In that case, DecodeRuneInString returns width=0.
			searchPos++
		} else {
			searchPos = a[1]
		}

		m, _ = re.FindNextMatch(m)
	}

	// Copy the unmatched characters after the last match.	
	buf = append(buf, src[lastMatchEnd:]...)

	return buf
}

// QuoteMeta returns a string that escapes all regular expression metacharacters
// inside the argument text; the returned string is a regular expression matching
// the literal text.
//
// Ported from https://golang.org/src/regexp/regexp.go
//
// TODO: verify that this quotes all correct characters in regexp2
func QuoteMeta(s string) string {
	// A byte loop is correct because all metacharacters are ASCII.
	var i int
	for i = 0; i < len(s); i++ {
		if special(s[i]) {
			break
		}
	}
	// No meta characters found, so return original string.
	if i >= len(s) {
		return s
	}

	b := make([]byte, 2*len(s)-i)
	copy(b, s[:i])
	j := i
	for ; i < len(s); i++ {
		if special(s[i]) {
			b[j] = '\\'
			j++
		}
		b[j] = s[i]
		j++
	}
	return string(b[:j])
}


// Bitmap used by func special to check whether a character needs to be escaped.
//
// Ported from https://golang.org/src/regexp/regexp.go
//
var specialBytes [16]byte

// special reports whether byte b needs to be escaped by QuoteMeta.
//
// Ported from https://golang.org/src/regexp/regexp.go
//
// TODO: verify that UTF8 can safely be used here
//
func special(b byte) bool {
	return b < utf8.RuneSelf && specialBytes[b%16]&(1<<(b/16)) != 0
}