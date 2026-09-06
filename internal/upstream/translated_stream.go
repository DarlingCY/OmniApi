package upstream

import (
	"net/http"
	"strings"

	"github.com/omniapi/omni-api/internal/model"
	"github.com/omniapi/omni-api/internal/translate"
)

func newConvertedStream(from model.Protocol, request model.Request, response *http.Response) (*Stream, error) {
	s := &Stream{response: response, protocol: from, lines: scanner(response), translation: translate.NewStream(from, request.Protocol, request.Model, request.RequestID, request.Raw)}
	frame, ok := s.NextFrame()
	if !ok {
		s.Close()
		if s.err != nil {
			return nil, s.err
		}
		return nil, &Failure{Status: 502, Message: "upstream stream ended before the first event"}
	}
	s.frames = append([]translate.Frame{frame}, s.frames...)
	return s, nil
}

func (s *Stream) Converted() bool { return s.translation != nil }

// SetError records a downstream write failure and lets request logging surface it.
func (s *Stream) SetError(err error) { s.err = err }

// NextFrame returns protocol events, including tool-only and reasoning events.
func (s *Stream) NextFrame() (translate.Frame, bool) {
	for {
		if len(s.frames) > 0 {
			f := s.frames[0]
			s.frames = s.frames[1:]
			return f, true
		}
		if s.done || s.err != nil || s.translation.Done() {
			return translate.Frame{}, false
		}
		f, ok := s.readFrame()
		if !ok {
			s.done = true
			if err := s.lines.Err(); err != nil {
				s.err = &Failure{Status: 502, Message: "upstream stream read failed", Cause: err}
				return translate.Frame{}, false
			}
			frames, err := s.translation.End()
			if err != nil {
				s.err = &Failure{Status: 502, Message: err.Error()}
				return translate.Frame{}, false
			}
			s.frames = frames
			continue
		}
		frames, err := s.translation.Push(f)
		if err != nil {
			s.err = &Failure{Status: 502, Message: err.Error()}
			return translate.Frame{}, false
		}
		s.frames = frames
	}
}

func (s *Stream) readFrame() (translate.Frame, bool) {
	f := translate.Frame{}
	data := []string{}
	seen := false
	for s.lines.Scan() {
		line := strings.TrimSuffix(s.lines.Text(), "\r")
		if line == "" {
			if !seen {
				continue
			}
			f.Data = strings.Join(data, "\n")
			return f, true
		}
		seen = true
		key, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch key {
		case "data":
			data = append(data, value)
		case "event":
			f.Event = value
		case "id":
			f.ID = value
		case "":
			if f.Comment != "" {
				f.Comment += "\n"
			}
			f.Comment += value
		}
	}
	if seen {
		f.Data = strings.Join(data, "\n")
		return f, true
	}
	return f, false
}
