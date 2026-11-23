package web

// The Changes tab: what GitCove will send to GitHub for this repo, in the order replay sends it.

import (
	"net/http"
	"strconv"
	"strings"
)

// change is one queued push or change to an issue or PR, as a sentence: "Author What On".
type change struct {
	Icon, Color    string // the octicon, without "i-", and its colour class
	Label          string // what kind of change, for the icon's tooltip
	Author, What   string
	Branch, Commit string // a push's
	Ref, Title     string // the issue or PR it's on, "" for a push
	Note           string // said after the issue or PR
	After          string // a muted aside after the time
	At, Status     string
	Admin          bool   // stuck in conflict or uncertain, for an admin to skip or retry
	Result         string // why, if Admin
}

func (s *Server) changes(w http.ResponseWriter, r *http.Request, p *page) error {
	ctx := r.Context()
	unsettled := []string{"pending", "sending", "conflict", "uncertain"}
	pushes, err := s.Store.RefItems(ctx, p.Repo.ID, unsettled...)
	if err != nil {
		return err
	}
	items, err := s.Store.MetaItems(ctx, p.Repo.ID, unsettled...)
	if err != nil {
		return err
	}
	mirrored, err := s.Store.Issues(ctx, p.Repo.ID)
	if err != nil {
		return err
	}
	titles, prs := map[string]string{}, map[string]bool{}
	for _, i := range mirrored {
		titles[strconv.Itoa(i.Number)], prs[strconv.Itoa(i.Number)] = i.Title, i.IsPR
	}
	for _, it := range items {
		if it.Kind == "issue" {
			titles[it.Issue], prs[it.Issue] = it.Change.Title, it.Change.Head != "" // a queued PR is an issue with a head branch
		}
	}
	stuck := func(state string) bool { return state == "conflict" || state == "uncertain" }
	var v struct{ Pushes, Items []change }
	for _, it := range pushes {
		what := "pushed to"
		if strings.Trim(it.Old, "0") == "" {
			what = "created"
		}
		v.Pushes = append(v.Pushes, change{Icon: "commit", Color: "muted", Label: "Push", Author: it.Pusher, What: what,
			Branch: it.Branch, Commit: it.New, At: it.Created, Status: status(it.State), Admin: stuck(it.State), Result: it.Result})
	}
	for _, it := range items {
		c, noun := it.Change, "issue"
		if prs[it.Issue] {
			noun = "pull request"
		}
		ch := change{Icon: "comment", Color: "muted", Author: c.Author, Ref: it.Issue, Title: titles[it.Issue], At: it.Created,
			Status: status(it.State), Admin: stuck(it.State), Result: it.Result}
		switch it.Kind {
		case "issue":
			ch.Icon, ch.Color, ch.Label, ch.What = "issue", "open", "New "+noun, "opened "+noun
			if prs[it.Issue] {
				ch.Icon = "pr"
			}
		case "comment":
			ch.Label, ch.What = "Comment", "commented on "+noun
		case "review":
			ch.Label, ch.What = "Review", "reviewed "+noun
			switch c.Verdict {
			case "approve":
				ch.Icon, ch.Color, ch.Label, ch.What = "check", "open", "Approval", "approved "+noun
			case "request_changes":
				ch.Icon, ch.Color, ch.Label, ch.What = "file-diff", "closed", "Changes requested", "requested changes on "+noun
			}
		case "reply":
			ch.Label, ch.What = "Reply", "replied to a line comment on "+noun
		case "merge":
			ch.Icon, ch.Color, ch.Label, ch.What = "merge", "done", "Merge on GitCove", "merged "+noun
			ch.Note, ch.After = "into "+c.Base+" on GitCove", "GitHub gets a note saying so, and a person merges it there."
		case "label":
			ch.Icon, ch.Label, ch.What = "tag", "Label", "removed the label "+c.Label+" from "+noun
			if c.Add {
				ch.What = "added the label " + c.Label + " to " + noun
			}
		case "state":
			ch.Icon, ch.Color, ch.Label, ch.What = "issue-closed", "done", "Close", "closed "+noun
			if c.State == "open" {
				ch.Icon, ch.Color, ch.Label, ch.What = "issue", "open", "Reopen", "reopened "+noun
			}
		case "edit":
			ch.Icon, ch.Label, ch.What = "pencil", "Edit", "edited the "+map[string]string{"title": "title", "body": "description"}[c.Field]+" of "+noun
		}
		v.Items = append(v.Items, ch)
	}
	p.Data = v
	return s.render(w, "changes", p)
}
