package hr

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/reqtz"
)

// Handler serves /api/v1/hr.
type Handler struct {
	DB  *gorm.DB
	Now func() time.Time
}

func NewHandler(db *gorm.DB) *Handler { return &Handler{DB: db} }

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// RegisterRoutes mounts the HR API on g (e.g. /api/v1/hr).
func (h *Handler) RegisterRoutes(g *gin.RouterGroup) {
	g.GET("/summary", h.summary)

	g.GET("/departments", h.listDepartments)
	g.POST("/departments", h.createDepartment)
	g.PUT("/departments/:id", h.updateDepartment)
	g.DELETE("/departments/:id", h.deleteDepartment)

	g.GET("/employees", h.listEmployees)
	g.POST("/employees", h.createEmployee)
	g.GET("/employees/:id", h.getEmployee)
	g.PUT("/employees/:id", h.updateEmployee)
	g.DELETE("/employees/:id", h.deleteEmployee)

	g.GET("/leave", h.listLeave)
	g.POST("/leave", h.createLeave)
	g.PUT("/leave/:id", h.updateLeave)
	g.DELETE("/leave/:id", h.deleteLeave)
	g.POST("/leave/:id/approve", h.decideLeave("approved"))
	g.POST("/leave/:id/reject", h.decideLeave("rejected"))

	g.GET("/timesheets", h.listTimesheets)
	g.POST("/timesheets", h.createTimesheet)
	g.PUT("/timesheets/:id", h.updateTimesheet)
	g.DELETE("/timesheets/:id", h.deleteTimesheet)
	g.POST("/timesheets/:id/approve", h.decideTimesheet("approved"))
	g.POST("/timesheets/:id/reject", h.decideTimesheet("rejected"))

	g.GET("/payruns", h.listPayRuns)
	g.POST("/payruns/preview", h.previewPayRun)
	g.POST("/payruns", h.createPayRun)
	g.GET("/payruns/:id", h.getPayRun)
	g.POST("/payruns/:id/recalculate", h.recalculatePayRun)
	g.POST("/payruns/:id/paid", h.markPaid)
	g.GET("/payruns/:id/export", h.exportPayRun)
	g.DELETE("/payruns/:id", h.deletePayRun)
}

// ---------------------------------------------------------------------------
// Summary

func (h *Handler) summary(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	now := h.now().In(reqtz.Location(c)) // the user's calendar (?tz=)
	today := now.Format(dateLayout)
	var active, onLeaveStatus, pendingLeave, pendingTs, depts, onLeaveToday int64
	h.DB.Model(&Employee{}).Where("org_id = ? AND status = ?", org, "active").Count(&active)
	h.DB.Model(&Employee{}).Where("org_id = ? AND status = ?", org, "on_leave").Count(&onLeaveStatus)
	h.DB.Model(&LeaveRequest{}).Where("org_id = ? AND status = ?", org, "pending").Count(&pendingLeave)
	h.DB.Model(&Timesheet{}).Where("org_id = ? AND status = ?", org, "submitted").Count(&pendingTs)
	h.DB.Model(&Department{}).Where("org_id = ?", org).Count(&depts)
	h.DB.Model(&LeaveRequest{}).Where("org_id = ? AND status = ? AND start_date <= ? AND end_date >= ?", org, "approved", today, today).
		Distinct("employee_id").Count(&onLeaveToday)

	weekStart := now.AddDate(0, 0, -int((now.Weekday()+6)%7)).Format(dateLayout)
	var hoursWeek float64
	h.DB.Model(&Timesheet{}).Where("org_id = ? AND status <> ? AND date >= ? AND date <= ?", org, "rejected", weekStart, today).
		Select("COALESCE(SUM(hours),0)").Scan(&hoursWeek)

	var last PayRun
	var lastRun interface{}
	if err := h.DB.Where("org_id = ? AND status = ?", org, "paid").Order("paid_at DESC").First(&last).Error; err == nil {
		lastRun = gin.H{"id": last.ID, "number": last.Number, "total_net": last.TotalNet, "total_gross": last.TotalGross, "currency": last.Currency, "pay_date": last.PayDate, "employee_count": last.EmployeeCount}
	}
	var drafts int64
	h.DB.Model(&PayRun{}).Where("org_id = ? AND status = ?", org, "draft").Count(&drafts)

	ok(c, http.StatusOK, gin.H{
		"headcount":          active + onLeaveStatus,
		"active":             active,
		"on_leave_today":     onLeaveToday,
		"pending_leave":      pendingLeave,
		"pending_timesheets": pendingTs,
		"hours_this_week":    hoursWeek,
		"departments":        depts,
		"draft_pay_runs":     drafts,
		"last_pay_run":       lastRun,
		"can_manage":         canManage(c),
	})
}

// ---------------------------------------------------------------------------
// Departments

type departmentInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (h *Handler) listDepartments(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	var rows []Department
	if err := h.DB.Where("org_id = ?", org).Order("name").Find(&rows).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not load departments")
		return
	}
	type cnt struct {
		DepartmentID string
		N            int64
	}
	var counts []cnt
	h.DB.Model(&Employee{}).Select("department_id, COUNT(*) AS n").
		Where("org_id = ? AND status <> ? AND department_id IS NOT NULL", org, "terminated").Group("department_id").Scan(&counts)
	m := map[string]int64{}
	for _, x := range counts {
		m[x.DepartmentID] = x.N
	}
	for i := range rows {
		rows[i].EmployeeCount = m[rows[i].ID]
	}
	ok(c, http.StatusOK, gin.H{"departments": rows})
}

func (h *Handler) departmentFromInput(c *gin.Context, org, exceptID string, d *Department) bool {
	var in departmentInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "Invalid request body")
		return false
	}
	name := trimMax(in.Name, 120)
	if name == "" {
		fail(c, http.StatusBadRequest, "Department name is required")
		return false
	}
	var n int64
	h.DB.Model(&Department{}).Where("org_id = ? AND LOWER(name) = ? AND id <> ?", org, strings.ToLower(name), exceptID).Count(&n)
	if n > 0 {
		fail(c, http.StatusConflict, "A department with that name already exists")
		return false
	}
	d.Name = name
	d.Description = trimMax(in.Description, 2000)
	return true
}

func (h *Handler) createDepartment(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk || !requireManager(c) {
		return
	}
	d := Department{OrgID: org}
	if !h.departmentFromInput(c, org, "", &d) {
		return
	}
	if err := h.DB.Create(&d).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not create department")
		return
	}
	ok(c, http.StatusCreated, d)
}

func (h *Handler) updateDepartment(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk || !requireManager(c) {
		return
	}
	var d Department
	if err := h.DB.Where("id = ? AND org_id = ?", c.Param("id"), org).First(&d).Error; err != nil {
		fail(c, http.StatusNotFound, "Department not found")
		return
	}
	if !h.departmentFromInput(c, org, d.ID, &d) {
		return
	}
	if err := h.DB.Save(&d).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not save department")
		return
	}
	ok(c, http.StatusOK, d)
}

func (h *Handler) deleteDepartment(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk || !requireManager(c) {
		return
	}
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Where("id = ? AND org_id = ?", c.Param("id"), org).Delete(&Department{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return tx.Model(&Employee{}).Where("org_id = ? AND department_id = ?", org, c.Param("id")).Update("department_id", nil).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		fail(c, http.StatusNotFound, "Department not found")
		return
	}
	if err != nil {
		fail(c, http.StatusInternalServerError, "Could not delete department")
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": true})
}

// ---------------------------------------------------------------------------
// Employees

type employeeInput struct {
	UserID         *string  `json:"user_id"`
	FirstName      string   `json:"first_name"`
	LastName       string   `json:"last_name"`
	Email          string   `json:"email"`
	Phone          string   `json:"phone"`
	Position       string   `json:"position"`
	DepartmentID   *string  `json:"department_id"`
	EmploymentType string   `json:"employment_type"`
	StartDate      string   `json:"start_date"`
	PayType        string   `json:"pay_type"`
	PayRate        *float64 `json:"pay_rate"`
	Currency       string   `json:"currency"`
	Status         string   `json:"status"`
	Notes          string   `json:"notes"`
}

func (h *Handler) decorate(org string, emps []Employee) {
	var depts []Department
	h.DB.Where("org_id = ?", org).Find(&depts)
	names := map[string]string{}
	for _, d := range depts {
		names[d.ID] = d.Name
	}
	for i := range emps {
		emps[i].FullName = fullName(emps[i].FirstName, emps[i].LastName)
		if emps[i].DepartmentID != nil {
			emps[i].DepartmentName = names[*emps[i].DepartmentID]
		}
	}
}

func (h *Handler) listEmployees(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	q := h.DB.Where("org_id = ?", org)
	if s := c.Query("status"); s != "" && s != "all" {
		q = q.Where("status = ?", s)
	}
	if d := c.Query("department_id"); d != "" {
		q = q.Where("department_id = ?", d)
	}
	if s := strings.ToLower(strings.TrimSpace(c.Query("q"))); s != "" {
		like := "%" + s + "%"
		q = q.Where("(LOWER(first_name) LIKE ? OR LOWER(last_name) LIKE ? OR LOWER(email) LIKE ? OR LOWER(position) LIKE ?)", like, like, like, like)
	}
	var rows []Employee
	if err := q.Order("first_name, last_name").Find(&rows).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not load employees")
		return
	}
	h.decorate(org, rows)
	ok(c, http.StatusOK, gin.H{"employees": rows, "total": len(rows)})
}

func (h *Handler) loadEmployee(c *gin.Context, org string) (*Employee, bool) {
	var e Employee
	if err := h.DB.Where("id = ? AND org_id = ?", c.Param("id"), org).First(&e).Error; err != nil {
		fail(c, http.StatusNotFound, "Employee not found")
		return nil, false
	}
	list := []Employee{e}
	h.decorate(org, list)
	return &list[0], true
}

func (h *Handler) getEmployee(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	e, found := h.loadEmployee(c, org)
	if !found {
		return
	}
	ok(c, http.StatusOK, e)
}

func (h *Handler) applyEmployee(c *gin.Context, org string, e *Employee) bool {
	var in employeeInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "Invalid request body")
		return false
	}
	e.FirstName = trimMax(in.FirstName, 100)
	e.LastName = trimMax(in.LastName, 100)
	if e.FirstName == "" {
		fail(c, http.StatusBadRequest, "First name is required")
		return false
	}
	e.Email = strings.ToLower(trimMax(in.Email, 255))
	if !validEmail(e.Email) {
		fail(c, http.StatusBadRequest, "Enter a valid email address")
		return false
	}
	e.Phone = trimMax(in.Phone, 40)
	e.Position = trimMax(in.Position, 120)
	e.Notes = trimMax(in.Notes, 5000)

	e.EmploymentType = strings.TrimSpace(in.EmploymentType)
	if e.EmploymentType == "" {
		e.EmploymentType = EmpFullTime
	}
	if !oneOf(e.EmploymentType, EmpFullTime, EmpPartTime, EmpCasual, EmpContractor) {
		fail(c, http.StatusBadRequest, "Employment type must be full time, part time, casual or contractor")
		return false
	}
	e.PayType = strings.TrimSpace(in.PayType)
	if e.PayType == "" {
		e.PayType = PayHourly
	}
	if !oneOf(e.PayType, PayHourly, PaySalary) {
		fail(c, http.StatusBadRequest, "Pay type must be hourly or salary")
		return false
	}
	if in.PayRate == nil || *in.PayRate < 0 {
		fail(c, http.StatusBadRequest, "Enter a pay rate of 0 or more")
		return false
	}
	e.PayRate = *in.PayRate
	cur := strings.ToUpper(strings.TrimSpace(in.Currency))
	if cur == "" {
		cur = orgprefs.Currency(h.DB, org)
	}
	if !currency.IsSupported(cur) {
		fail(c, http.StatusBadRequest, "Unsupported currency "+cur)
		return false
	}
	e.Currency = cur
	e.Status = strings.TrimSpace(in.Status)
	if e.Status == "" {
		e.Status = "active"
	}
	if !oneOf(e.Status, "active", "on_leave", "terminated") {
		fail(c, http.StatusBadRequest, "Status must be active, on leave or terminated")
		return false
	}
	e.StartDate = strings.TrimSpace(in.StartDate)
	if e.StartDate != "" && !validDate(e.StartDate) {
		fail(c, http.StatusBadRequest, "Start date must be YYYY-MM-DD")
		return false
	}
	e.DepartmentID = nil
	if in.DepartmentID != nil && strings.TrimSpace(*in.DepartmentID) != "" {
		id := strings.TrimSpace(*in.DepartmentID)
		var n int64
		h.DB.Model(&Department{}).Where("id = ? AND org_id = ?", id, org).Count(&n)
		if n == 0 {
			fail(c, http.StatusBadRequest, "Department not found")
			return false
		}
		e.DepartmentID = &id
	}
	e.UserID = nil
	if in.UserID != nil && strings.TrimSpace(*in.UserID) != "" {
		id := strings.TrimSpace(*in.UserID)
		var n int64
		h.DB.Table("users").Where("id = ? AND organization_id = ? AND deleted_at IS NULL", id, org).Count(&n)
		if n == 0 {
			fail(c, http.StatusBadRequest, "Linked user not found in your organisation")
			return false
		}
		var dup int64
		h.DB.Model(&Employee{}).Where("org_id = ? AND user_id = ? AND id <> ?", org, id, e.ID).Count(&dup)
		if dup > 0 {
			fail(c, http.StatusConflict, "That user is already linked to another employee")
			return false
		}
		e.UserID = &id
	}
	if e.Email != "" {
		var dup int64
		h.DB.Model(&Employee{}).Where("org_id = ? AND email = ? AND id <> ?", org, e.Email, e.ID).Count(&dup)
		if dup > 0 {
			fail(c, http.StatusConflict, "Another employee already uses that email")
			return false
		}
	}
	return true
}

func (h *Handler) createEmployee(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk || !requireManager(c) {
		return
	}
	e := Employee{OrgID: org}
	if !h.applyEmployee(c, org, &e) {
		return
	}
	if err := h.DB.Create(&e).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not create employee")
		return
	}
	list := []Employee{e}
	h.decorate(org, list)
	ok(c, http.StatusCreated, list[0])
}

func (h *Handler) updateEmployee(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk || !requireManager(c) {
		return
	}
	var e Employee
	if err := h.DB.Where("id = ? AND org_id = ?", c.Param("id"), org).First(&e).Error; err != nil {
		fail(c, http.StatusNotFound, "Employee not found")
		return
	}
	if !h.applyEmployee(c, org, &e) {
		return
	}
	if err := h.DB.Save(&e).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not save employee")
		return
	}
	list := []Employee{e}
	h.decorate(org, list)
	ok(c, http.StatusOK, list[0])
}

func (h *Handler) deleteEmployee(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk || !requireManager(c) {
		return
	}
	var e Employee
	if err := h.DB.Where("id = ? AND org_id = ?", c.Param("id"), org).First(&e).Error; err != nil {
		fail(c, http.StatusNotFound, "Employee not found")
		return
	}
	var paid int64
	h.DB.Model(&PayRunLine{}).Joins("JOIN hr_pay_runs ON hr_pay_runs.id = hr_pay_run_lines.pay_run_id").
		Where("hr_pay_run_lines.employee_id = ? AND hr_pay_runs.status = ?", e.ID, "paid").Count(&paid)
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if paid == 0 {
			// Never paid: remove their pending records too.
			if err := tx.Where("org_id = ? AND employee_id = ?", org, e.ID).Delete(&LeaveRequest{}).Error; err != nil {
				return err
			}
			if err := tx.Where("org_id = ? AND employee_id = ?", org, e.ID).Delete(&Timesheet{}).Error; err != nil {
				return err
			}
		}
		return tx.Delete(&e).Error
	})
	if err != nil {
		fail(c, http.StatusInternalServerError, "Could not delete employee")
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": true})
}

// employeeNames maps employee ids to display names (including deleted ones).
func (h *Handler) employeeNames(org string) map[string]string {
	var emps []Employee
	h.DB.Unscoped().Select("id, first_name, last_name").Where("org_id = ?", org).Find(&emps)
	m := make(map[string]string, len(emps))
	for _, e := range emps {
		m[e.ID] = fullName(e.FirstName, e.LastName)
	}
	return m
}

func (h *Handler) employeeInOrg(org, id string) (*Employee, bool) {
	var e Employee
	if id == "" || h.DB.Where("id = ? AND org_id = ?", id, org).First(&e).Error != nil {
		return nil, false
	}
	return &e, true
}

// ---------------------------------------------------------------------------
// Leave

var leaveTypes = []string{"annual", "sick", "personal", "parental", "unpaid", "other"}

type leaveInput struct {
	EmployeeID string `json:"employee_id"`
	Type       string `json:"type"`
	StartDate  string `json:"start_date"`
	EndDate    string `json:"end_date"`
	Reason     string `json:"reason"`
}

func (h *Handler) listLeave(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	q := h.DB.Where("org_id = ?", org)
	if s := c.Query("status"); s != "" && s != "all" {
		q = q.Where("status = ?", s)
	}
	if e := c.Query("employee_id"); e != "" {
		q = q.Where("employee_id = ?", e)
	}
	var rows []LeaveRequest
	if err := q.Order("start_date DESC, created_at DESC").Limit(500).Find(&rows).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not load leave requests")
		return
	}
	names := h.employeeNames(org)
	for i := range rows {
		rows[i].EmployeeName = names[rows[i].EmployeeID]
	}
	ok(c, http.StatusOK, gin.H{"leave": rows})
}

func (h *Handler) applyLeave(c *gin.Context, org string, l *LeaveRequest) bool {
	var in leaveInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "Invalid request body")
		return false
	}
	if _, found := h.employeeInOrg(org, in.EmployeeID); !found {
		fail(c, http.StatusBadRequest, "Choose an employee")
		return false
	}
	if !oneOf(in.Type, leaveTypes...) {
		fail(c, http.StatusBadRequest, "Choose a leave type")
		return false
	}
	s, err1 := parseDate(in.StartDate)
	e, err2 := parseDate(in.EndDate)
	if err1 != nil || err2 != nil {
		fail(c, http.StatusBadRequest, "Enter valid start and end dates")
		return false
	}
	if e.Before(s) {
		fail(c, http.StatusBadRequest, "End date can't be before the start date")
		return false
	}
	if e.Sub(s) > 366*24*time.Hour {
		fail(c, http.StatusBadRequest, "Leave can't be longer than a year")
		return false
	}
	days := LeaveDays(s, e)
	if days == 0 {
		fail(c, http.StatusBadRequest, "The selected dates only cover a weekend")
		return false
	}
	var overlap int64
	h.DB.Model(&LeaveRequest{}).Where("org_id = ? AND employee_id = ? AND status <> ? AND id <> ? AND start_date <= ? AND end_date >= ?",
		org, in.EmployeeID, "rejected", l.ID, in.EndDate, in.StartDate).Count(&overlap)
	if overlap > 0 {
		fail(c, http.StatusConflict, "This overlaps another leave request for the same employee")
		return false
	}
	l.EmployeeID = in.EmployeeID
	l.Type = in.Type
	l.StartDate = s.Format(dateLayout)
	l.EndDate = e.Format(dateLayout)
	l.Days = days
	l.Reason = trimMax(in.Reason, 2000)
	return true
}

func (h *Handler) createLeave(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	l := LeaveRequest{OrgID: org, Status: "pending"}
	if !h.applyLeave(c, org, &l) {
		return
	}
	if err := h.DB.Create(&l).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not save leave request")
		return
	}
	l.EmployeeName = h.employeeNames(org)[l.EmployeeID]
	ok(c, http.StatusCreated, l)
}

func (h *Handler) updateLeave(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	var l LeaveRequest
	if err := h.DB.Where("id = ? AND org_id = ?", c.Param("id"), org).First(&l).Error; err != nil {
		fail(c, http.StatusNotFound, "Leave request not found")
		return
	}
	if l.Status != "pending" {
		fail(c, http.StatusConflict, "Only pending requests can be edited")
		return
	}
	if !h.applyLeave(c, org, &l) {
		return
	}
	if err := h.DB.Save(&l).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not save leave request")
		return
	}
	l.EmployeeName = h.employeeNames(org)[l.EmployeeID]
	ok(c, http.StatusOK, l)
}

func (h *Handler) deleteLeave(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	var l LeaveRequest
	if err := h.DB.Where("id = ? AND org_id = ?", c.Param("id"), org).First(&l).Error; err != nil {
		fail(c, http.StatusNotFound, "Leave request not found")
		return
	}
	if l.Status != "pending" && !canManage(c) {
		fail(c, http.StatusForbidden, "Only managers can delete decided requests")
		return
	}
	h.DB.Delete(&l)
	ok(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *Handler) decideLeave(status string) gin.HandlerFunc {
	return func(c *gin.Context) {
		org, okk := orgOf(c)
		if !okk || !requireManager(c) {
			return
		}
		var l LeaveRequest
		if err := h.DB.Where("id = ? AND org_id = ?", c.Param("id"), org).First(&l).Error; err != nil {
			fail(c, http.StatusNotFound, "Leave request not found")
			return
		}
		if l.Status == status {
			fail(c, http.StatusConflict, "This request is already "+status)
			return
		}
		now := h.now()
		l.Status = status
		l.DecidedBy = ctxString(c, "user_id")
		l.DecidedAt = &now
		if err := h.DB.Save(&l).Error; err != nil {
			fail(c, http.StatusInternalServerError, "Could not update leave request")
			return
		}
		l.EmployeeName = h.employeeNames(org)[l.EmployeeID]
		ok(c, http.StatusOK, l)
	}
}

// ---------------------------------------------------------------------------
// Timesheets

type timesheetInput struct {
	EmployeeID string   `json:"employee_id"`
	Date       string   `json:"date"`
	Hours      *float64 `json:"hours"`
	Notes      string   `json:"notes"`
}

func (h *Handler) listTimesheets(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	q := h.DB.Where("org_id = ?", org)
	if s := c.Query("status"); s != "" && s != "all" {
		q = q.Where("status = ?", s)
	}
	if e := c.Query("employee_id"); e != "" {
		q = q.Where("employee_id = ?", e)
	}
	if f := c.Query("from"); validDate(f) {
		q = q.Where("date >= ?", f)
	}
	if t := c.Query("to"); validDate(t) {
		q = q.Where("date <= ?", t)
	}
	var rows []Timesheet
	if err := q.Order("date DESC, created_at DESC").Limit(1000).Find(&rows).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not load timesheets")
		return
	}
	names := h.employeeNames(org)
	var total float64
	for i := range rows {
		rows[i].EmployeeName = names[rows[i].EmployeeID]
		if rows[i].Status != "rejected" {
			total += rows[i].Hours
		}
	}
	ok(c, http.StatusOK, gin.H{"timesheets": rows, "total_hours": total})
}

func (h *Handler) applyTimesheet(c *gin.Context, org string, t *Timesheet) bool {
	var in timesheetInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "Invalid request body")
		return false
	}
	if _, found := h.employeeInOrg(org, in.EmployeeID); !found {
		fail(c, http.StatusBadRequest, "Choose an employee")
		return false
	}
	d, err := parseDate(in.Date)
	if err != nil {
		fail(c, http.StatusBadRequest, "Enter a valid date")
		return false
	}
	if d.After(h.now().AddDate(0, 0, 1)) {
		fail(c, http.StatusBadRequest, "Timesheets can't be in the future")
		return false
	}
	if in.Hours == nil || *in.Hours <= 0 || *in.Hours > 24 {
		fail(c, http.StatusBadRequest, "Hours must be more than 0 and at most 24")
		return false
	}
	var sum float64
	h.DB.Model(&Timesheet{}).Where("org_id = ? AND employee_id = ? AND date = ? AND status <> ? AND id <> ?", org, in.EmployeeID, d.Format(dateLayout), "rejected", t.ID).
		Select("COALESCE(SUM(hours),0)").Scan(&sum)
	if sum+*in.Hours > 24 {
		fail(c, http.StatusBadRequest, fmt.Sprintf("That would be more than 24 hours on %s (already %.2g logged)", d.Format(dateLayout), sum))
		return false
	}
	t.EmployeeID = in.EmployeeID
	t.Date = d.Format(dateLayout)
	t.Hours = RoundTo(*in.Hours, "USD")
	t.Notes = trimMax(in.Notes, 2000)
	return true
}

func (h *Handler) createTimesheet(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	t := Timesheet{OrgID: org, Status: "submitted"}
	if !h.applyTimesheet(c, org, &t) {
		return
	}
	if err := h.DB.Create(&t).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not save timesheet")
		return
	}
	t.EmployeeName = h.employeeNames(org)[t.EmployeeID]
	ok(c, http.StatusCreated, t)
}

func (h *Handler) loadTimesheet(c *gin.Context, org string) (*Timesheet, bool) {
	var t Timesheet
	if err := h.DB.Where("id = ? AND org_id = ?", c.Param("id"), org).First(&t).Error; err != nil {
		fail(c, http.StatusNotFound, "Timesheet not found")
		return nil, false
	}
	if t.PayRunID != nil {
		fail(c, http.StatusConflict, "This timesheet has been paid and is locked")
		return nil, false
	}
	return &t, true
}

func (h *Handler) updateTimesheet(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	t, found := h.loadTimesheet(c, org)
	if !found {
		return
	}
	if t.Status == "approved" && !canManage(c) {
		fail(c, http.StatusForbidden, "Approved timesheets can only be changed by a manager")
		return
	}
	if !h.applyTimesheet(c, org, t) {
		return
	}
	t.Status = "submitted"
	t.ApprovedAt = nil
	t.ApprovedBy = ""
	if err := h.DB.Save(t).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not save timesheet")
		return
	}
	t.EmployeeName = h.employeeNames(org)[t.EmployeeID]
	ok(c, http.StatusOK, t)
}

func (h *Handler) deleteTimesheet(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	t, found := h.loadTimesheet(c, org)
	if !found {
		return
	}
	if t.Status == "approved" && !canManage(c) {
		fail(c, http.StatusForbidden, "Approved timesheets can only be deleted by a manager")
		return
	}
	h.DB.Delete(t)
	ok(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *Handler) decideTimesheet(status string) gin.HandlerFunc {
	return func(c *gin.Context) {
		org, okk := orgOf(c)
		if !okk || !requireManager(c) {
			return
		}
		t, found := h.loadTimesheet(c, org)
		if !found {
			return
		}
		now := h.now()
		t.Status = status
		t.ApprovedBy = ctxString(c, "user_id")
		t.ApprovedAt = &now
		if err := h.DB.Save(t).Error; err != nil {
			fail(c, http.StatusInternalServerError, "Could not update timesheet")
			return
		}
		t.EmployeeName = h.employeeNames(org)[t.EmployeeID]
		ok(c, http.StatusOK, t)
	}
}

// ---------------------------------------------------------------------------
// Pay runs

type payRunInput struct {
	Frequency     string   `json:"frequency"`
	PeriodStart   string   `json:"period_start"`
	PayDate       string   `json:"pay_date"`
	Currency      string   `json:"currency"`
	DeductionRate *float64 `json:"deduction_rate"`
	EmployeeIDs   []string `json:"employee_ids"`
	Notes         string   `json:"notes"`
}

type payRunPlan struct {
	Run     PayRun
	Lines   []PayRunLine
	Skipped []gin.H
}

// plan validates the input and computes every line. It doesn't write anything.
func (h *Handler) plan(org string, in payRunInput, existingID string) (*payRunPlan, int, error) {
	if !oneOf(in.Frequency, FreqWeekly, FreqFortnightly, FreqMonthly) {
		return nil, http.StatusBadRequest, errors.New("Choose a pay frequency")
	}
	start, err := parseDate(in.PeriodStart)
	if err != nil {
		return nil, http.StatusBadRequest, errors.New("Enter a valid period start date")
	}
	end := PeriodEnd(start, in.Frequency)
	payDate := strings.TrimSpace(in.PayDate)
	if payDate == "" {
		payDate = end.Format(dateLayout)
	} else if !validDate(payDate) {
		return nil, http.StatusBadRequest, errors.New("Enter a valid pay date")
	}
	cur := strings.ToUpper(strings.TrimSpace(in.Currency))
	if cur == "" {
		cur = orgprefs.Currency(h.DB, org)
	}
	if !currency.IsSupported(cur) {
		return nil, http.StatusBadRequest, errors.New("Unsupported currency " + cur)
	}
	rate := 0.0
	if in.DeductionRate != nil {
		rate = *in.DeductionRate
	}
	if rate < 0 || rate > 100 {
		return nil, http.StatusBadRequest, errors.New("Deductions must be between 0% and 100%")
	}

	q := h.DB.Where("org_id = ? AND currency = ? AND status <> ?", org, cur, "terminated")
	if len(in.EmployeeIDs) > 0 {
		q = q.Where("id IN ?", in.EmployeeIDs)
	}
	var emps []Employee
	if err := q.Order("first_name, last_name").Find(&emps).Error; err != nil {
		return nil, http.StatusInternalServerError, errors.New("Could not load employees")
	}
	if len(emps) == 0 {
		return nil, http.StatusBadRequest, fmt.Errorf("No active employees are paid in %s", cur)
	}

	// Approved, unpaid hours per employee in the period.
	type hrs struct {
		EmployeeID string
		Hours      float64
	}
	var sums []hrs
	h.DB.Model(&Timesheet{}).Select("employee_id, SUM(hours) AS hours").
		Where("org_id = ? AND status = ? AND date >= ? AND date <= ? AND pay_run_id IS NULL", org, "approved", start.Format(dateLayout), end.Format(dateLayout)).
		Group("employee_id").Scan(&sums)
	hours := map[string]float64{}
	for _, s := range sums {
		hours[s.EmployeeID] = s.Hours
	}

	// Salaried staff can't be paid twice for the same period.
	var already []string
	h.DB.Model(&PayRunLine{}).Joins("JOIN hr_pay_runs ON hr_pay_runs.id = hr_pay_run_lines.pay_run_id").
		Where("hr_pay_runs.org_id = ? AND hr_pay_runs.id <> ? AND hr_pay_runs.period_start <= ? AND hr_pay_runs.period_end >= ? AND hr_pay_run_lines.pay_type = ?",
			org, existingID, end.Format(dateLayout), start.Format(dateLayout), PaySalary).
		Pluck("hr_pay_run_lines.employee_id", &already)
	dup := map[string]bool{}
	for _, id := range already {
		dup[id] = true
	}

	p := &payRunPlan{Run: PayRun{
		OrgID: org, Frequency: in.Frequency, PeriodStart: start.Format(dateLayout), PeriodEnd: end.Format(dateLayout),
		PayDate: payDate, Currency: cur, DeductionRate: rate, Notes: trimMax(in.Notes, 2000), Status: "draft",
	}}
	for _, e := range emps {
		name := fullName(e.FirstName, e.LastName)
		if e.PayType == PaySalary && dup[e.ID] {
			p.Skipped = append(p.Skipped, gin.H{"employee_id": e.ID, "name": name, "reason": "Already in another pay run for this period"})
			continue
		}
		li := LineInput{PayType: e.PayType, Rate: e.PayRate, Hours: hours[e.ID], Frequency: in.Frequency, DeductionRate: rate, Currency: cur}
		if e.PayType == PaySalary {
			li.Hours = 0
		}
		res, err := CalcLine(li)
		if err != nil {
			p.Skipped = append(p.Skipped, gin.H{"employee_id": e.ID, "name": name, "reason": err.Error()})
			continue
		}
		if res.Gross == 0 {
			reason := "No pay rate set"
			if e.PayType == PayHourly && li.Hours == 0 {
				reason = "No approved hours in this period"
			}
			p.Skipped = append(p.Skipped, gin.H{"employee_id": e.ID, "name": name, "reason": reason})
			continue
		}
		p.Lines = append(p.Lines, PayRunLine{
			EmployeeID: e.ID, EmployeeName: name, Position: e.Position, PayType: e.PayType,
			Hours: li.Hours, Rate: e.PayRate, Gross: res.Gross, Deductions: res.Deductions, Net: res.Net,
		})
	}
	p.Run.TotalGross, p.Run.TotalDeductions, p.Run.TotalNet = Totals(p.Lines, cur)
	p.Run.EmployeeCount = len(p.Lines)
	return p, 0, nil
}

func (h *Handler) listPayRuns(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	q := h.DB.Where("org_id = ?", org)
	if s := c.Query("status"); s != "" && s != "all" {
		q = q.Where("status = ?", s)
	}
	var rows []PayRun
	if err := q.Order("period_start DESC, created_at DESC").Limit(200).Find(&rows).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not load pay runs")
		return
	}
	ok(c, http.StatusOK, gin.H{"pay_runs": rows})
}

func (h *Handler) previewPayRun(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk || !requireManager(c) {
		return
	}
	var in payRunInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "Invalid request body")
		return
	}
	p, status, err := h.plan(org, in, "")
	if err != nil {
		fail(c, status, err.Error())
		return
	}
	p.Run.Lines = p.Lines
	ok(c, http.StatusOK, gin.H{"pay_run": p.Run, "skipped": p.Skipped})
}

func (h *Handler) nextNumber(tx *gorm.DB, org string) string {
	var n int64
	tx.Unscoped().Model(&PayRun{}).Where("org_id = ?", org).Count(&n)
	for {
		n++
		num := fmt.Sprintf("PR-%04d", n)
		var dup int64
		tx.Model(&PayRun{}).Where("org_id = ? AND number = ?", org, num).Count(&dup)
		if dup == 0 {
			return num
		}
	}
}

func (h *Handler) createPayRun(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk || !requireManager(c) {
		return
	}
	var in payRunInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "Invalid request body")
		return
	}
	p, status, err := h.plan(org, in, "")
	if err != nil {
		fail(c, status, err.Error())
		return
	}
	if len(p.Lines) == 0 {
		fail(c, http.StatusBadRequest, "Nobody has anything to be paid for this period. Approve timesheets or check pay rates first.")
		return
	}
	run := p.Run
	run.CreatedBy = ctxString(c, "user_id")
	err = h.DB.Transaction(func(tx *gorm.DB) error {
		run.Number = h.nextNumber(tx, org)
		if err := tx.Omit("Lines").Create(&run).Error; err != nil {
			return err
		}
		for i := range p.Lines {
			p.Lines[i].PayRunID = run.ID
		}
		return tx.Create(&p.Lines).Error
	})
	if err != nil {
		fail(c, http.StatusInternalServerError, "Could not create pay run")
		return
	}
	run.Lines = p.Lines
	ok(c, http.StatusCreated, gin.H{"pay_run": run, "skipped": p.Skipped})
}

func (h *Handler) loadPayRun(c *gin.Context, org string) (*PayRun, bool) {
	var r PayRun
	if err := h.DB.Preload("Lines", func(db *gorm.DB) *gorm.DB { return db.Order("employee_name") }).
		Where("id = ? AND org_id = ?", c.Param("id"), org).First(&r).Error; err != nil {
		fail(c, http.StatusNotFound, "Pay run not found")
		return nil, false
	}
	return &r, true
}

func (h *Handler) getPayRun(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	r, found := h.loadPayRun(c, org)
	if !found {
		return
	}
	ok(c, http.StatusOK, r)
}

func (h *Handler) recalculatePayRun(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk || !requireManager(c) {
		return
	}
	r, found := h.loadPayRun(c, org)
	if !found {
		return
	}
	if r.Status != "draft" {
		fail(c, http.StatusConflict, "Only draft pay runs can be recalculated")
		return
	}
	var body struct {
		DeductionRate *float64 `json:"deduction_rate"`
		PayDate       string   `json:"pay_date"`
		Notes         *string  `json:"notes"`
	}
	_ = c.ShouldBindJSON(&body)
	in := payRunInput{Frequency: r.Frequency, PeriodStart: r.PeriodStart, PayDate: r.PayDate, Currency: r.Currency, DeductionRate: &r.DeductionRate, Notes: r.Notes}
	if body.DeductionRate != nil {
		in.DeductionRate = body.DeductionRate
	}
	if body.PayDate != "" {
		in.PayDate = body.PayDate
	}
	if body.Notes != nil {
		in.Notes = *body.Notes
	}
	for _, l := range r.Lines {
		in.EmployeeIDs = append(in.EmployeeIDs, l.EmployeeID)
	}
	if len(in.EmployeeIDs) == 0 {
		in.EmployeeIDs = nil
	}
	p, status, err := h.plan(org, in, r.ID)
	if err != nil {
		fail(c, status, err.Error())
		return
	}
	err = h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("pay_run_id = ?", r.ID).Delete(&PayRunLine{}).Error; err != nil {
			return err
		}
		for i := range p.Lines {
			p.Lines[i].PayRunID = r.ID
		}
		if len(p.Lines) > 0 {
			if err := tx.Create(&p.Lines).Error; err != nil {
				return err
			}
		}
		return tx.Model(&PayRun{}).Where("id = ?", r.ID).Updates(map[string]interface{}{
			"deduction_rate": p.Run.DeductionRate, "pay_date": p.Run.PayDate, "notes": p.Run.Notes,
			"total_gross": p.Run.TotalGross, "total_deductions": p.Run.TotalDeductions, "total_net": p.Run.TotalNet,
			"employee_count": p.Run.EmployeeCount,
		}).Error
	})
	if err != nil {
		fail(c, http.StatusInternalServerError, "Could not recalculate pay run")
		return
	}
	r, _ = h.loadPayRun(c, org)
	ok(c, http.StatusOK, gin.H{"pay_run": r, "skipped": p.Skipped})
}

func (h *Handler) markPaid(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk || !requireManager(c) {
		return
	}
	r, found := h.loadPayRun(c, org)
	if !found {
		return
	}
	if r.Status == "paid" {
		fail(c, http.StatusConflict, "This pay run is already paid")
		return
	}
	if len(r.Lines) == 0 {
		fail(c, http.StatusBadRequest, "This pay run has no employees")
		return
	}
	// Hourly pay must still match the approved, unpaid timesheets.
	type hrs struct {
		EmployeeID string
		Hours      float64
	}
	var sums []hrs
	h.DB.Model(&Timesheet{}).Select("employee_id, SUM(hours) AS hours").
		Where("org_id = ? AND status = ? AND date >= ? AND date <= ? AND pay_run_id IS NULL", org, "approved", r.PeriodStart, r.PeriodEnd).
		Group("employee_id").Scan(&sums)
	current := map[string]float64{}
	for _, s := range sums {
		current[s.EmployeeID] = s.Hours
	}
	for _, l := range r.Lines {
		if l.PayType == PayHourly && RoundTo(current[l.EmployeeID], "USD") != RoundTo(l.Hours, "USD") {
			fail(c, http.StatusConflict, "Timesheets for "+l.EmployeeName+" changed since this pay run was calculated. Recalculate it first.")
			return
		}
	}
	now := h.now()
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		hourly := []string{}
		for _, l := range r.Lines {
			if l.PayType == PayHourly {
				hourly = append(hourly, l.EmployeeID)
			}
		}
		if len(hourly) > 0 {
			// Lock in the hours we paid so they can't be paid again.
			if err := tx.Model(&Timesheet{}).Where("org_id = ? AND employee_id IN ? AND status = ? AND date >= ? AND date <= ? AND pay_run_id IS NULL",
				org, hourly, "approved", r.PeriodStart, r.PeriodEnd).Update("pay_run_id", r.ID).Error; err != nil {
				return err
			}
		}
		return tx.Model(&PayRun{}).Where("id = ?", r.ID).Updates(map[string]interface{}{"status": "paid", "paid_at": now}).Error
	})
	if err != nil {
		fail(c, http.StatusInternalServerError, "Could not mark the pay run as paid")
		return
	}
	r, _ = h.loadPayRun(c, org)
	ok(c, http.StatusOK, r)
}

func (h *Handler) deletePayRun(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk || !requireManager(c) {
		return
	}
	r, found := h.loadPayRun(c, org)
	if !found {
		return
	}
	if r.Status == "paid" {
		fail(c, http.StatusConflict, "Paid pay runs can't be deleted")
		return
	}
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("pay_run_id = ?", r.ID).Delete(&PayRunLine{}).Error; err != nil {
			return err
		}
		return tx.Delete(&PayRun{}, "id = ?", r.ID).Error
	})
	if err != nil {
		fail(c, http.StatusInternalServerError, "Could not delete pay run")
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": true})
}

// PayRunCSV renders a pay run as CSV (one row per employee plus a totals row).
func PayRunCSV(r *PayRun) []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	dec := currency.Decimals(r.Currency)
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', dec, 64) }
	_ = w.Write([]string{"Pay run", "Period start", "Period end", "Pay date", "Employee", "Position", "Pay type", "Hours", "Rate", "Gross", "Deductions", "Net", "Currency"})
	lines := append([]PayRunLine(nil), r.Lines...)
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].EmployeeName < lines[j].EmployeeName })
	for _, l := range lines {
		hours := ""
		if l.PayType == PayHourly {
			hours = strconv.FormatFloat(l.Hours, 'f', 2, 64)
		}
		_ = w.Write([]string{r.Number, r.PeriodStart, r.PeriodEnd, r.PayDate, csvSafe(l.EmployeeName), csvSafe(l.Position), l.PayType, hours,
			strconv.FormatFloat(l.Rate, 'f', -1, 64), f(l.Gross), f(l.Deductions), f(l.Net), r.Currency})
	}
	_ = w.Write([]string{r.Number, r.PeriodStart, r.PeriodEnd, r.PayDate, "TOTAL", "", "", "", "", f(r.TotalGross), f(r.TotalDeductions), f(r.TotalNet), r.Currency})
	w.Flush()
	return buf.Bytes()
}

// csvSafe neutralises spreadsheet formula injection.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func (h *Handler) exportPayRun(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	r, found := h.loadPayRun(c, org)
	if !found {
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.csv"`, r.Number))
	c.Data(http.StatusOK, "text/csv; charset=utf-8", PayRunCSV(r))
}
