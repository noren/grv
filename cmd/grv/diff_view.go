package main

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
	"sync"

	log "github.com/Sirupsen/logrus"
)

type diffViewHandler func(*DiffView, Action) error

type diffLineType int

const (
	dltUnset diffLineType = iota
	dltNormal
	dltDiffCommitAuthor
	dltDiffCommitAuthorDate
	dltDiffCommitCommitter
	dltDiffCommitCommitterDate
	dltDiffCommitMessage
	dltDiffStatsFile
	dltGitDiffHeader
	dltGitDiffExtendedHeader
	dltUnifiedDiffHeader
	dltHunkStart
	dltLineAdded
	dltLineRemoved
)

const (
	dvDateFormat                 = "Mon Jan 2 15:04:05 2006 -0700"
	dvDiffLoadRequestChannelSize = 100
)

var diffLineThemeComponentID = map[diffLineType]ThemeComponentID{
	dltNormal:                  CmpDiffviewDifflineNormal,
	dltDiffCommitAuthor:        CmpDiffviewDifflineDiffCommitAuthor,
	dltDiffCommitAuthorDate:    CmpDiffviewDifflineDiffCommitAuthorDate,
	dltDiffCommitCommitter:     CmpDiffviewDifflineDiffCommitCommitter,
	dltDiffCommitCommitterDate: CmpDiffviewDifflineDiffCommitCommitterDate,
	dltDiffCommitMessage:       CmpDiffviewDifflineDiffCommitMessage,
	dltDiffStatsFile:           CmpDiffviewDifflineDiffStatsFile,
	dltGitDiffHeader:           CmpDiffviewDifflineGitDiffHeader,
	dltGitDiffExtendedHeader:   CmpDiffviewDifflineGitDiffExtendedHeader,
	dltUnifiedDiffHeader:       CmpDiffviewDifflineUnifiedDiffHeader,
	dltHunkStart:               CmpDiffviewDifflineHunkStart,
	dltLineAdded:               CmpDiffviewDifflineLineAdded,
	dltLineRemoved:             CmpDiffviewDifflineLineRemoved,
}

type diffLineData struct {
	line     string
	lineType diffLineType
}

func (diffLine *diffLineData) getThemeComponentID() ThemeComponentID {
	diffLine.determineDiffLineType()
	return diffLineThemeComponentID[diffLine.lineType]
}

func (diffLine *diffLineData) determineDiffLineType() {
	if diffLine.lineType != dltUnset {
		return
	}

	var lineType diffLineType
	line := diffLine.line

	switch {
	case strings.HasPrefix(line, "diff --git"):
		lineType = dltGitDiffHeader
	case strings.HasPrefix(line, "index"):
		lineType = dltGitDiffExtendedHeader
	case strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++"):
		lineType = dltUnifiedDiffHeader
	case strings.HasPrefix(line, "@@"):
		lineType = dltHunkStart
	case strings.HasPrefix(line, "+"):
		lineType = dltLineAdded
	case strings.HasPrefix(line, "-"):
		lineType = dltLineRemoved
	default:
		lineType = dltNormal
	}

	diffLine.lineType = lineType
}

type diffLines struct {
	lines   []*diffLineData
	viewPos ViewPos
}

type diffLoadRequest interface {
	diffLoadRequest()
}

type commitDiffLoadRequest struct {
	commit *Commit
}

type fileDiffLoadRequest struct {
	statusType StatusType
	filePath   string
}

type stageDiffLoadRequest struct {
	statusType StatusType
}

func (request *commitDiffLoadRequest) diffLoadRequest() {}
func (request *fileDiffLoadRequest) diffLoadRequest()   {}
func (request *stageDiffLoadRequest) diffLoadRequest()  {}

type diffID string

// DiffView contains all state for the diff view
type DiffView struct {
	channels          *Channels
	repoData          RepoData
	config            Config
	activeDiff        diffID
	diffs             map[diffID]*diffLines
	viewPos           ViewPos
	viewDimension     ViewDimension
	handlers          map[ActionType]diffViewHandler
	active            bool
	viewSearch        *ViewSearch
	diffLoadRequestCh chan diffLoadRequest
	lock              sync.Mutex
}

// NewDiffView creates a new diff view instance
func NewDiffView(repoData RepoData, channels *Channels, config Config) *DiffView {
	diffView := &DiffView{
		repoData:          repoData,
		channels:          channels,
		config:            config,
		viewPos:           NewViewPosition(),
		diffs:             make(map[diffID]*diffLines),
		diffLoadRequestCh: make(chan diffLoadRequest, dvDiffLoadRequestChannelSize),
		handlers: map[ActionType]diffViewHandler{
			ActionPrevLine:           moveUpDiffLine,
			ActionNextLine:           moveDownDiffLine,
			ActionPrevPage:           moveUpDiffPage,
			ActionNextPage:           moveDownDiffPage,
			ActionPrevHalfPage:       moveUpDiffHalfPage,
			ActionNextHalfPage:       moveDownDiffHalfPage,
			ActionScrollRight:        scrollDiffViewRight,
			ActionScrollLeft:         scrollDiffViewLeft,
			ActionFirstLine:          moveToFirstDiffLine,
			ActionLastLine:           moveToLastDiffLine,
			ActionCenterView:         centerDiffView,
			ActionScrollCursorTop:    scrollDiffViewTop,
			ActionScrollCursorBottom: scrollDiffViewBottom,
			ActionCursorTopView:      moveCursorTopDiffView,
			ActionCursorMiddleView:   moveCursorMiddleDiffView,
			ActionCursorBottomView:   moveCursorBottomDiffView,
			ActionSelect:             selectDiffLine,
			ActionMouseSelect:        mouseSelectDiffLine,
			ActionMouseScrollDown:    mouseScrollDownDiffView,
			ActionMouseScrollUp:      mouseScrollUpDiffView,
		},
	}

	diffView.viewSearch = NewViewSearch(diffView, channels)

	return diffView
}

// Initialise does nothing
func (diffView *DiffView) Initialise() (err error) {
	log.Info("Initialising DiffView")

	diffView.lock.Lock()
	defer diffView.lock.Unlock()

	go diffView.processDiffLoadRequests()

	return
}

// Dispose of any resources held by the view
func (diffView *DiffView) Dispose() {
	diffView.lock.Lock()
	defer diffView.lock.Unlock()

	close(diffView.diffLoadRequestCh)
}

// Render generates and writes the diff view to the provided window
func (diffView *DiffView) Render(win RenderWindow) (err error) {
	diffView.lock.Lock()
	defer diffView.lock.Unlock()

	diffView.viewDimension = win.ViewDimensions()

	if diffView.activeDiff == "" {
		return diffView.renderEmptyView(win, "No diff to display")
	} else if len(diffView.diffLoadRequestCh) > 0 {
		return diffView.renderEmptyView(win, "Loading diff...")
	}

	rows := win.Rows() - 2
	viewPos := diffView.viewPos
	diffLines, ok := diffView.diffs[diffView.activeDiff]
	if !ok {
		log.Errorf("No diff data found for %v", diffView.activeDiff)
		return
	}

	lineNum := uint(len(diffLines.lines))
	viewPos.DetermineViewStartRow(rows, lineNum)

	lineIndex := viewPos.ViewStartRowIndex()
	startColumn := viewPos.ViewStartColumn()

	for rowIndex := uint(0); rowIndex < rows && lineIndex < lineNum; rowIndex++ {
		diffLine := diffLines.lines[lineIndex]
		themeComponentID := diffLine.getThemeComponentID()

		if diffLine.lineType == dltHunkStart {
			lineParts := strings.SplitAfter(diffLine.line, "@@")

			if len(lineParts) != 3 {
				return fmt.Errorf("Unable to display hunk header line: %v", diffLine.line)
			}

			var lineBuilder *LineBuilder
			if lineBuilder, err = win.LineBuilder(rowIndex+1, startColumn); err != nil {
				return
			}

			lineBuilder.
				AppendWithStyle(themeComponentID, " %v", strings.Join(lineParts[:2], "")).
				AppendWithStyle(CmpDiffviewDifflineHunkHeader, "%v", lineParts[2])

		} else if diffLine.lineType == dltDiffStatsFile {
			sepIndex := strings.LastIndex(diffLine.line, "|")

			if sepIndex == -1 || sepIndex >= len(diffLine.line)-1 {
				return fmt.Errorf("Unable to display diff stats file line: %v", diffLine.line)
			}

			filePart := diffLine.line[0:sepIndex]
			changePart := diffLine.line[sepIndex+1:]

			var lineBuilder *LineBuilder
			if lineBuilder, err = win.LineBuilder(rowIndex+1, startColumn); err != nil {
				return
			}

			lineBuilder.AppendWithStyle(CmpDiffviewDifflineDiffStatsFile, " %v |", filePart)

			for _, char := range changePart {
				switch char {
				case '+':
					lineBuilder.AppendWithStyle(CmpDiffviewDifflineLineAdded, "%c", char)
				case '-':
					lineBuilder.AppendWithStyle(CmpDiffviewDifflineLineRemoved, "%c", char)
				default:
					lineBuilder.Append("%c", char)
				}
			}
		} else if err = win.SetRow(rowIndex+1, startColumn, themeComponentID, " %v", diffLines.lines[lineIndex].line); err != nil {
			return
		}

		lineIndex++
	}

	if err = win.SetSelectedRow(viewPos.SelectedRowIndex()+1, diffView.active); err != nil {
		return
	}

	win.DrawBorder()

	if err = win.SetTitle(CmpDiffviewTitle, "Diff for %v", diffView.activeDiff); err != nil {
		return
	}

	if err = win.SetFooter(CmpDiffviewFooter, "Line %v of %v", viewPos.ActiveRowIndex()+1, lineNum); err != nil {
		return
	}

	if searchActive, searchPattern, lastSearchFoundMatch := diffView.viewSearch.SearchActive(); searchActive && lastSearchFoundMatch {
		if err = win.Highlight(searchPattern, CmpAllviewSearchMatch); err != nil {
			return
		}
	}

	return
}

func (diffView *DiffView) renderEmptyView(win RenderWindow, msg string) (err error) {
	viewPos := diffView.viewPos
	startColumn := viewPos.ViewStartColumn()

	if err = win.SetRow(2, startColumn, CmpNone, "   %v", msg); err != nil {
		return
	}

	win.DrawBorder()

	return
}

// RenderHelpBar renders help information for the diff view
func (diffView *DiffView) RenderHelpBar(lineBuilder *LineBuilder) (err error) {
	diffView.lock.Lock()
	defer diffView.lock.Unlock()

	if diffView.activeDiff == "" {
		return
	}

	diffLines, ok := diffView.diffs[diffView.activeDiff]
	if !ok {
		return
	}

	lineIndex := diffView.viewPos.ActiveRowIndex()

	if lineIndex < uint(len(diffLines.lines)) {
		line := diffLines.lines[lineIndex]

		if line.lineType == dltDiffStatsFile {
			RenderKeyBindingHelp(diffView.ViewID(), lineBuilder, []ActionMessage{
				{action: ActionSelect, message: "Jump to file diff"},
			})
		}
	}

	return
}

// OnActiveChange sets whether the diff view is the active view or not
func (diffView *DiffView) OnActiveChange(active bool) {
	log.Debugf("DiffView active: %v", active)
	diffView.lock.Lock()
	defer diffView.lock.Unlock()

	diffView.active = active
}

// ViewID returns the diff views ID
func (diffView *DiffView) ViewID() ViewID {
	return ViewDiff
}

// OnCommitSelected loads/fetches the diff for the selected commit and refreshes the display
func (diffView *DiffView) OnCommitSelected(commit *Commit) (err error) {
	log.Debugf("DiffView loading diff for selected commit %v", commit.commit.Id())

	diffID := diffID(commit.oid.String())

	diffView.lock.Lock()
	if diffLines, ok := diffView.diffs[diffID]; ok {
		diffView.activeDiff = diffID
		diffView.viewPos = diffLines.viewPos
		diffView.channels.UpdateDisplay()
		diffView.lock.Unlock()
		return
	}

	diffView.lock.Unlock()

	diffView.addDiffLoadRequest(&commitDiffLoadRequest{
		commit: commit,
	})

	return
}

// OnFileSelected loads/fetches the diff for the selected file and refreshes the display
func (diffView *DiffView) OnFileSelected(statusType StatusType, filePath string) {
	log.Debugf("DiffView loading diff for file %v", filePath)

	diffView.addDiffLoadRequest(&fileDiffLoadRequest{
		statusType: statusType,
		filePath:   filePath,
	})
}

// OnStageGroupSelected does nothing
func (diffView *DiffView) OnStageGroupSelected(statusType StatusType) {
	log.Debugf("DiffView loading diff for stage %v", statusType)

	diffView.addDiffLoadRequest(&stageDiffLoadRequest{
		statusType: statusType,
	})
}

// OnNoEntrySelected clears the diff view
func (diffView *DiffView) OnNoEntrySelected() {
	log.Debugf("No entry selected to display diff for")

	diffView.lock.Lock()
	defer diffView.lock.Unlock()

	diffView.activeDiff = diffID("")
	diffView.channels.UpdateDisplay()
}

func (diffView *DiffView) addDiffLoadRequest(request diffLoadRequest) {
	diffView.diffLoadRequestCh <- request
}

func (diffView *DiffView) processDiffLoadRequests() {
	for request := range diffView.diffLoadRequestCh {
		request = diffView.retrieveLatestDiffLoadRequest(request)
		var err error

		switch req := request.(type) {
		case *commitDiffLoadRequest:
			err = diffView.loadCommitDiffAndMakeActive(req)
		case *fileDiffLoadRequest:
			err = diffView.loadFileDiffAndMakeActive(req)
		case *stageDiffLoadRequest:
			err = diffView.loadStageDiffAndMakeActive(req)
		default:
			log.Errorf("Unknown diff load request type: %T", request)
		}

		if err != nil {
			diffView.channels.ReportError(err)
		}

		diffView.channels.UpdateDisplay()
	}
}

func (diffView *DiffView) retrieveLatestDiffLoadRequest(request diffLoadRequest) diffLoadRequest {
	requestFound := true

	for requestFound {
		select {
		case request = <-diffView.diffLoadRequestCh:
		default:
			requestFound = false
		}
	}

	return request
}

func (diffView *DiffView) loadCommitDiffAndMakeActive(request *commitDiffLoadRequest) (err error) {
	commit := request.commit
	diffID := diffID(commit.oid.String())

	lines, err := diffView.generateDiffLinesForCommit(commit)
	if err != nil {
		return
	}

	diffLines := &diffLines{
		lines:   lines,
		viewPos: NewViewPosition(),
	}

	diffView.lock.Lock()
	defer diffView.lock.Unlock()

	diffView.activeDiff = diffID
	diffView.diffs[diffID] = diffLines
	diffView.viewPos = diffLines.viewPos

	return
}

func (diffView *DiffView) loadFileDiffAndMakeActive(request *fileDiffLoadRequest) (err error) {
	statusType := request.statusType
	filePath := request.filePath

	diff, err := diffView.repoData.DiffFile(statusType, filePath)
	if err != nil {
		log.Errorf("Unable to load file diff: %v", err)
		return
	}

	if err = diffView.storeDiff(diffID(filePath), diff); err != nil {
		log.Errorf("Unable to store file diff: %v", err)
		return
	}

	return
}

func (diffView *DiffView) loadStageDiffAndMakeActive(request *stageDiffLoadRequest) (err error) {
	statusType := request.statusType

	diff, err := diffView.repoData.DiffStage(statusType)
	if err != nil {
		log.Errorf("Unable to load diff for stage %v: %v", statusType, err)
		return
	}

	id := fmt.Sprintf("%v files", strings.ToLower(StatusTypeDisplayName(statusType)))
	if err = diffView.storeDiff(diffID(id), diff); err != nil {
		log.Errorf("Unable to store stage diff: %v", err)
		return
	}

	return
}

func (diffView *DiffView) storeDiff(diffID diffID, diff *Diff) (err error) {
	lines, err := diffView.generateDiffLinesForDiff(diff)
	if err != nil {
		return
	}

	diffLines := &diffLines{
		lines:   lines,
		viewPos: NewViewPosition(),
	}

	diffView.lock.Lock()
	defer diffView.lock.Unlock()

	diffView.diffs[diffID] = diffLines
	diffView.activeDiff = diffID
	diffView.viewPos = diffLines.viewPos

	return
}

func (diffView *DiffView) generateDiffLinesForCommit(commit *Commit) (lines []*diffLineData, err error) {
	author := commit.commit.Author()
	committer := commit.commit.Committer()

	lines = append(lines,
		&diffLineData{
			line:     fmt.Sprintf("Author:\t%v <%v>", author.Name, author.Email),
			lineType: dltDiffCommitAuthor,
		},
		&diffLineData{
			line:     fmt.Sprintf("AuthorDate:\t%v", author.When.Format(dvDateFormat)),
			lineType: dltDiffCommitAuthorDate,
		},
		&diffLineData{
			line:     fmt.Sprintf("Committer:\t%v <%v>", committer.Name, committer.Email),
			lineType: dltDiffCommitCommitter,
		},
		&diffLineData{
			line:     fmt.Sprintf("CommitterDate:\t%v", committer.When.Format(dvDateFormat)),
			lineType: dltDiffCommitCommitterDate,
		},
		&diffLineData{
			lineType: dltNormal,
		},
	)

	commitMessageScanner := bufio.NewScanner(strings.NewReader(commit.commit.Message()))

	for commitMessageScanner.Scan() {
		lines = append(lines, &diffLineData{
			line:     commitMessageScanner.Text(),
			lineType: dltDiffCommitMessage,
		})
	}

	lines = append(lines, &diffLineData{
		lineType: dltNormal,
	})

	diff, err := diffView.repoData.DiffCommit(commit)
	if err != nil {
		return
	}

	diffContent, err := diffView.generateDiffLinesForDiff(diff)
	if err != nil {
		return
	}

	lines = append(lines, diffContent...)

	return
}

func (diffView *DiffView) generateDiffLinesForDiff(diff *Diff) (lines []*diffLineData, err error) {
	scanner := bufio.NewScanner(bytes.NewReader(diff.stats.Bytes()))

	for scanner.Scan() {
		lines = append(lines, &diffLineData{
			line:     strings.TrimPrefix(scanner.Text(), " "),
			lineType: dltDiffStatsFile,
		})
	}

	if len(lines) > 0 {
		prevLine := lines[len(lines)-1]

		if prevLine.lineType == dltDiffStatsFile {
			prevLine.lineType = dltNormal
		}

		lines = append(lines, &diffLineData{
			lineType: dltNormal,
		})
	}

	scanner = bufio.NewScanner(bytes.NewReader(diff.diffText.Bytes()))

	for scanner.Scan() {
		lines = append(lines, &diffLineData{
			line: scanner.Text(),
		})
	}

	return
}

// HandleEvent does nothing
func (diffView *DiffView) HandleEvent(event Event) (err error) {
	return
}

// ViewPos returns the current view position
func (diffView *DiffView) ViewPos() ViewPos {
	return diffView.viewPos
}

// OnSearchMatch sets the current view position to the search match position
func (diffView *DiffView) OnSearchMatch(startPos ViewPos, matchLineIndex uint) {
	diffView.lock.Lock()
	defer diffView.lock.Unlock()

	viewPos := diffView.ViewPos()

	if viewPos != startPos {
		log.Debugf("Selected ref has changed since search started")
		return
	}

	viewPos.SetActiveRowIndex(matchLineIndex)
}

// HandleAction checks if the diff view supports the provided action and executes it if so
func (diffView *DiffView) HandleAction(action Action) (err error) {
	log.Debugf("DiffView handling action %v", action)
	diffView.lock.Lock()
	defer diffView.lock.Unlock()

	if handler, ok := diffView.handlers[action.ActionType]; ok {
		err = handler(diffView, action)
	} else {
		_, err = diffView.viewSearch.HandleAction(action)
	}

	return
}

// Line returns the rendered line from the diff view at the specified line index
func (diffView *DiffView) Line(lineIndex uint) (line string) {
	diffView.lock.Lock()
	defer diffView.lock.Unlock()

	diffLines, ok := diffView.diffs[diffView.activeDiff]
	if !ok {
		return
	}

	lineNum := uint(len(diffLines.lines))

	if lineIndex >= lineNum {
		log.Errorf("Invalid lineIndex: %v", lineIndex)
		return
	}

	diffLine := diffLines.lines[lineIndex]
	line = diffLine.line

	return
}

// LineNumber returns the number of lines the diff view currently has
func (diffView *DiffView) LineNumber() (lineNumber uint) {
	diffView.lock.Lock()
	defer diffView.lock.Unlock()

	return diffView.lineNumber()
}

func (diffView *DiffView) lineNumber() (lineNumber uint) {
	diffLines, ok := diffView.diffs[diffView.activeDiff]
	if !ok {
		return
	}

	lineNum := uint(len(diffLines.lines))

	return lineNum
}

func moveDownDiffLine(diffView *DiffView, action Action) (err error) {
	diffLines, ok := diffView.diffs[diffView.activeDiff]
	if !ok {
		return
	}

	lineNum := uint(len(diffLines.lines))
	viewPos := diffView.viewPos

	if viewPos.MoveLineDown(lineNum) {
		log.Debugf("Moving down one line in diff view")
		diffView.channels.UpdateDisplay()
	}

	return
}

func moveUpDiffLine(diffView *DiffView, action Action) (err error) {
	viewPos := diffView.viewPos

	if viewPos.MoveLineUp() {
		log.Debugf("Moving up one line in diff view")
		diffView.channels.UpdateDisplay()
	}

	return
}

func moveDownDiffPage(diffView *DiffView, action Action) (err error) {
	diffLines, ok := diffView.diffs[diffView.activeDiff]
	if !ok {
		return
	}

	lineNum := uint(len(diffLines.lines))
	viewPos := diffView.viewPos

	if viewPos.MovePageDown(diffView.viewDimension.rows-2, lineNum) {
		log.Debugf("Moving down one page in diff view")
		diffView.channels.UpdateDisplay()
	}

	return
}

func moveUpDiffPage(diffView *DiffView, action Action) (err error) {
	viewPos := diffView.viewPos

	if viewPos.MovePageUp(diffView.viewDimension.rows - 2) {
		log.Debugf("Moving up one page in diff view")
		diffView.channels.UpdateDisplay()
	}

	return
}

func moveDownDiffHalfPage(diffView *DiffView, action Action) (err error) {
	diffLines, ok := diffView.diffs[diffView.activeDiff]
	if !ok {
		return
	}

	lineNum := uint(len(diffLines.lines))
	viewPos := diffView.viewPos

	if viewPos.MovePageDown(diffView.viewDimension.rows/2-2, lineNum) {
		log.Debugf("Moving down one page in diff view")
		diffView.channels.UpdateDisplay()
	}

	return
}

func moveUpDiffHalfPage(diffView *DiffView, action Action) (err error) {
	viewPos := diffView.viewPos

	if viewPos.MovePageUp(diffView.viewDimension.rows/2 - 2) {
		log.Debugf("Moving up one page in diff view")
		diffView.channels.UpdateDisplay()
	}

	return
}

func scrollDiffViewRight(diffView *DiffView, action Action) (err error) {
	viewPos := diffView.viewPos
	viewPos.MovePageRight(diffView.viewDimension.cols)
	log.Debugf("Scrolling right. View starts at column %v", viewPos.ViewStartColumn())
	diffView.channels.UpdateDisplay()

	return
}

func scrollDiffViewLeft(diffView *DiffView, action Action) (err error) {
	viewPos := diffView.viewPos

	if viewPos.MovePageLeft(diffView.viewDimension.cols) {
		log.Debugf("Scrolling left. View starts at column %v", viewPos.ViewStartColumn())
		diffView.channels.UpdateDisplay()
	}

	return
}

func moveToFirstDiffLine(diffView *DiffView, action Action) (err error) {
	viewPos := diffView.viewPos

	if viewPos.MoveToFirstLine() {
		log.Debugf("Moving to first line in diff view")
		diffView.channels.UpdateDisplay()
	}

	return
}

func moveToLastDiffLine(diffView *DiffView, action Action) (err error) {
	diffLines, ok := diffView.diffs[diffView.activeDiff]
	if !ok {
		return
	}

	lineNum := uint(len(diffLines.lines))
	viewPos := diffView.viewPos

	if viewPos.MoveToLastLine(lineNum) {
		log.Debugf("Moving to last line in diff view")
		diffView.channels.UpdateDisplay()
	}

	return
}

func centerDiffView(diffView *DiffView, action Action) (err error) {
	viewPos := diffView.viewPos

	if viewPos.CenterActiveRow(diffView.viewDimension.rows - 2) {
		log.Debug("Centering DiffView")
		diffView.channels.UpdateDisplay()
	}

	return
}

func scrollDiffViewTop(diffView *DiffView, action Action) (err error) {
	viewPos := diffView.ViewPos()

	if viewPos.ScrollActiveRowTop() {
		log.Debug("Scrolling DiffView to make curor on top")
		diffView.channels.UpdateDisplay()
	}

	return
}

func scrollDiffViewBottom(diffView *DiffView, action Action) (err error) {
	viewPos := diffView.ViewPos()

	if viewPos.ScrollActiveRowBottom(diffView.viewDimension.rows - 2) {
		log.Debug("Scrolling DiffView to make curor on bottom")
		diffView.channels.UpdateDisplay()
	}

	return
}

func moveCursorTopDiffView(diffView *DiffView, action Action) (err error) {
	viewPos := diffView.ViewPos()

	if viewPos.MoveCursorTopPage() {
		log.Debug("Moving Cursor to top of diff view")
		diffView.channels.UpdateDisplay()
	}

	return
}

func moveCursorMiddleDiffView(diffView *DiffView, action Action) (err error) {
	diffLines, ok := diffView.diffs[diffView.activeDiff]
	if !ok {
		return
	}
	lineNumber := uint(len(diffLines.lines))

	viewPos := diffView.ViewPos()

	if viewPos.MoveCursorMiddlePage(diffView.viewDimension.rows-2, lineNumber) {
		log.Debug("Moving Cursor to middle of diff view")
		diffView.channels.UpdateDisplay()
	}

	return
}

func moveCursorBottomDiffView(diffView *DiffView, action Action) (err error) {
	diffLines, ok := diffView.diffs[diffView.activeDiff]
	if !ok {
		return
	}
	lineNumber := uint(len(diffLines.lines))

	viewPos := diffView.ViewPos()

	if viewPos.MoveCursorBottomPage(diffView.viewDimension.rows-2, lineNumber) {
		log.Debug("Moving Cursor to bottom of diff view")
		diffView.channels.UpdateDisplay()
	}

	return
}

func selectDiffLine(diffView *DiffView, action Action) (err error) {
	diffLines, ok := diffView.diffs[diffView.activeDiff]
	if !ok {
		return
	}

	lineIndex := diffView.viewPos.ActiveRowIndex()
	diffLine := diffLines.lines[lineIndex]

	if diffLine.lineType != dltDiffStatsFile {
		return
	}

	sepIndex := strings.LastIndex(diffLine.line, "|")

	if sepIndex == -1 || sepIndex >= len(diffLine.line)-1 {
		return fmt.Errorf("Unable to determine file path from line: %v", diffLine.line)
	}

	filePart := strings.TrimRight(diffLine.line[0:sepIndex], " ")
	pattern := fmt.Sprintf("diff --git a/%v b/%v", filePart, filePart)

	for lineIndex++; lineIndex < uint(len(diffLines.lines)); lineIndex++ {
		diffLine = diffLines.lines[lineIndex]

		if strings.HasPrefix(diffLine.line, pattern) {
			break
		}
	}

	if lineIndex >= uint(len(diffLines.lines)) {
		return fmt.Errorf("Unable to find diff for file: %v", filePart)
	}

	diffView.viewPos.SetActiveRowIndex(lineIndex)
	defer diffView.channels.UpdateDisplay()

	return centerDiffView(diffView, action)
}

func mouseSelectDiffLine(diffView *DiffView, action Action) (err error) {
	mouseEvent, err := GetMouseEventFromAction(action)
	if err != nil {
		return
	}

	if mouseEvent.row == 0 || mouseEvent.row == diffView.viewDimension.rows-1 {
		return
	}

	viewPos := diffView.viewPos
	selectedIndex := viewPos.ViewStartRowIndex() + mouseEvent.row - 1

	diffLines, ok := diffView.diffs[diffView.activeDiff]
	if !ok {
		return
	}

	if selectedIndex >= uint(len(diffLines.lines)) {
		return
	}

	diffView.viewPos.SetActiveRowIndex(selectedIndex)
	diffView.channels.UpdateDisplay()

	return
}

func mouseScrollDownDiffView(diffView *DiffView, action Action) (err error) {
	viewPos := diffView.viewPos
	lineNumber := diffView.lineNumber()
	pageRows := diffView.viewDimension.rows - 2
	scrollRows := uint(diffView.config.GetInt(CfMouseScrollRows))

	if viewPos.ScrollDown(lineNumber, pageRows, scrollRows) {
		diffView.channels.UpdateDisplay()
	}

	return
}

func mouseScrollUpDiffView(diffView *DiffView, action Action) (err error) {
	viewPos := diffView.viewPos
	pageRows := diffView.viewDimension.rows - 2
	scrollRows := uint(diffView.config.GetInt(CfMouseScrollRows))

	if viewPos.ScrollUp(pageRows, scrollRows) {
		diffView.channels.UpdateDisplay()
	}

	return
}
