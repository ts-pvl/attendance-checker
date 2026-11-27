package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"attendance/internal/core"
)

// Points for social activities (core/points.go): to everyone present from
// the Today tab, to one student from the student page. Admins only.

// coreError turns the errors staff can cause into a message in the UI's
// language; anything else is shown as it is.
func (s *Server) coreError(err error) string {
	var taken *core.LoginTakenError
	switch {
	case errors.Is(err, core.ErrBadPoints):
		return s.t("points.bad_points", core.MaxPoints)
	case errors.Is(err, core.ErrNoReason):
		return s.t("points.no_reason")
	case errors.Is(err, core.ErrNobodyHere):
		return s.t("points.nobody")
	case errors.Is(err, core.ErrNoEntry):
		return s.t("points.no_entry")
	case errors.Is(err, core.ErrBadPlatformLogin):
		return s.t("login.bad")
	case errors.As(err, &taken):
		return s.t("login.taken", taken.Login, taken.Owner)
	}
	return s.t("c.error", err.Error())
}

func formPoints(r *http.Request) int {
	n, err := strconv.Atoi(r.PostFormValue("points"))
	if err != nil {
		return 0 // refused by the core as out of range
	}
	return n
}

func (s *Server) handlePointsPresent(w http.ResponseWriter, r *http.Request) {
	points := formPoints(r)
	n, err := core.AwardPresent(r.Context(), s.DB, s.Now(), points, r.PostFormValue("reason"), staffFrom(r).Username)
	if err != nil {
		back(w, r, "/admin/today", s.coreError(err), true)
		return
	}
	back(w, r, "/admin/today", s.t("points.added_present", points, n), false)
}

func (s *Server) handlePointsStudent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	dest := fmt.Sprintf("/admin/students/%d", id)
	if err := core.AwardStudent(r.Context(), s.DB, s.Now(), id, formPoints(r), r.PostFormValue("reason"), staffFrom(r).Username); err != nil {
		back(w, r, dest, s.coreError(err), true)
		return
	}
	back(w, r, dest, s.t("points.added_student", formPoints(r)), false)
}

func (s *Server) handlePointsCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	student, err := core.CancelPoints(r.Context(), s.DB, s.Now(), id, staffFrom(r).Username)
	if err != nil {
		back(w, r, "/admin/students", s.coreError(err), true)
		return
	}
	back(w, r, fmt.Sprintf("/admin/students/%d", student), s.t("points.cancelled_one"), false)
}

func (s *Server) handlePointsBatchCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	n, err := core.CancelBatch(r.Context(), s.DB, s.Now(), id, staffFrom(r).Username)
	if err != nil {
		back(w, r, "/admin/today", s.coreError(err), true)
		return
	}
	back(w, r, "/admin/today", s.t("points.cancelled_batch", n), false)
}
