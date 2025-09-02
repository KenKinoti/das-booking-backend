package handlers

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// DashboardStats represents overview statistics
type DashboardStats struct {
	TotalParticipants   int64   `json:"total_participants"`
	ActiveParticipants  int64   `json:"active_participants"`
	TotalStaff         int64   `json:"total_staff"`
	ActiveStaff        int64   `json:"active_staff"`
	TotalShifts        int     `json:"total_shifts"`
	CompletedShifts    int     `json:"completed_shifts"`
	ScheduledShifts    int     `json:"scheduled_shifts"`
	TotalRevenue       float64 `json:"total_revenue"`
	MonthlyRevenue     float64 `json:"monthly_revenue"`
	ServiceHours       float64 `json:"service_hours"`
	TodayHours         float64 `json:"today_hours"`
	WeekHours          float64 `json:"week_hours"`
	RecentActivities   []RecentActivity `json:"recent_activities"`
}

type RecentActivity struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"` // shift_completed, participant_added, etc.
	Description string    `json:"description"`
	Timestamp   time.Time `json:"timestamp"`
	UserID      string    `json:"user_id,omitempty"`
	UserName    string    `json:"user_name,omitempty"`
}

func (h *Handler) GetDashboardStats(c *gin.Context) {
	orgID, exists := c.Get("org_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "UNAUTHORIZED",
				"message": "Organization not found in context",
			},
		})
		return
	}

	stats := DashboardStats{}

	// Get participant counts
	h.DB.Model(&models.Participant{}).Where("organization_id = ? AND deleted_at IS NULL", orgID).Count(&stats.TotalParticipants)
	h.DB.Model(&models.Participant{}).Where("organization_id = ? AND is_active = ? AND deleted_at IS NULL", orgID, true).Count(&stats.ActiveParticipants)

	// Get staff counts
	h.DB.Model(&models.User{}).Where("organization_id = ? AND deleted_at IS NULL", orgID).Count(&stats.TotalStaff)
	h.DB.Model(&models.User{}).Where("organization_id = ? AND is_active = ? AND deleted_at IS NULL", orgID, true).Count(&stats.ActiveStaff)

	// Get shift statistics
	var shifts []models.Shift
	h.DB.Joins("JOIN participants ON shifts.participant_id = participants.id").
		Where("participants.organization_id = ?", orgID).Find(&shifts)

	stats.TotalShifts = len(shifts)
	
	// Enhanced service hours tracking
	todayHours := 0.0
	weekHours := 0.0
	now := time.Now()
	startOfWeek := now.AddDate(0, 0, -int(now.Weekday())+1) // Monday
	
	for _, shift := range shifts {
		if shift.Status == "completed" {
			stats.CompletedShifts++
			// Calculate revenue from completed shifts
			hours := shift.EndTime.Sub(shift.StartTime).Hours()
			revenue := hours * shift.HourlyRate
			stats.TotalRevenue += revenue
			stats.ServiceHours += hours
			
			// Today's hours
			if shift.StartTime.Format("2006-01-02") == now.Format("2006-01-02") {
				todayHours += hours
			}
			
			// This week's hours
			if shift.StartTime.After(startOfWeek) || shift.StartTime.Equal(startOfWeek) {
				weekHours += hours
			}
			
			// Check if shift is from current month
			if shift.StartTime.Year() == now.Year() && shift.StartTime.Month() == now.Month() {
				stats.MonthlyRevenue += revenue
			}
		} else if shift.Status == "scheduled" {
			stats.ScheduledShifts++
		}
	}

	// Set the calculated hours in stats
	stats.TodayHours = todayHours
	stats.WeekHours = weekHours

	// Generate recent activities (mock data for MVP)
	stats.RecentActivities = []RecentActivity{
		{
			ID:          "activity_1",
			Type:        "shift_completed",
			Description: "Shift completed for John Doe",
			Timestamp:   time.Now().Add(-2 * time.Hour),
			UserName:    "Sarah Wilson",
		},
		{
			ID:          "activity_2",
			Type:        "participant_added",
			Description: "New participant registered: Jane Smith",
			Timestamp:   time.Now().Add(-4 * time.Hour),
			UserName:    "Admin User",
		},
		{
			ID:          "activity_3",
			Type:        "shift_scheduled",
			Description: "New shift scheduled for tomorrow",
			Timestamp:   time.Now().Add(-6 * time.Hour),
			UserName:    "Manager",
		},
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    stats,
	})
}

func (h *Handler) GetRevenueReport(c *gin.Context) {
	orgID, exists := c.Get("org_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "UNAUTHORIZED",
				"message": "Organization not found in context",
			},
		})
		return
	}

	// Get completed shifts for revenue calculation
	var shifts []models.Shift
	h.DB.Joins("JOIN participants ON shifts.participant_id = participants.id").
		Where("participants.organization_id = ? AND shifts.status = ?", orgID, "completed").
		Preload("Participant").Find(&shifts)

	// Calculate monthly revenue breakdown
	monthlyRevenue := make(map[string]float64)
	totalRevenue := 0.0

	for _, shift := range shifts {
		hours := shift.EndTime.Sub(shift.StartTime).Hours()
		revenue := hours * shift.HourlyRate
		totalRevenue += revenue
		
		monthKey := shift.StartTime.Format("2006-01")
		monthlyRevenue[monthKey] += revenue
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"total_revenue":    totalRevenue,
			"monthly_revenue":  monthlyRevenue,
			"shifts_included":  len(shifts),
			"report_generated": time.Now(),
		},
	})
}

func (h *Handler) GetShiftsReport(c *gin.Context) {
	orgID, exists := c.Get("org_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "UNAUTHORIZED",
				"message": "Organization not found in context",
			},
		})
		return
	}

	// Get all shifts
	var shifts []models.Shift
	h.DB.Joins("JOIN participants ON shifts.participant_id = participants.id").
		Where("participants.organization_id = ?", orgID).
		Preload("Participant").Preload("Staff").Find(&shifts)

	// Analyze shifts by status
	statusBreakdown := make(map[string]int)
	serviceTypeBreakdown := make(map[string]int)
	totalHours := 0.0

	for _, shift := range shifts {
		statusBreakdown[shift.Status]++
		serviceTypeBreakdown[shift.ServiceType]++
		
		if shift.Status == "completed" {
			hours := shift.EndTime.Sub(shift.StartTime).Hours()
			totalHours += hours
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"total_shifts":           len(shifts),
			"status_breakdown":       statusBreakdown,
			"service_type_breakdown": serviceTypeBreakdown,
			"total_service_hours":    totalHours,
			"report_generated":       time.Now(),
		},
	})
}

func (h *Handler) GetServiceHoursReport(c *gin.Context) {
	orgID, exists := c.Get("org_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "UNAUTHORIZED",
				"message": "Organization not found in context",
			},
		})
		return
	}

	// Get completed shifts for hours calculation
	var shifts []models.Shift
	h.DB.Joins("JOIN participants ON shifts.participant_id = participants.id").
		Where("participants.organization_id = ? AND shifts.status = ?", orgID, "completed").
		Preload("Participant").Preload("Staff").Find(&shifts)

	// Enhanced analytics calculations
	participantHours := make(map[string]float64)
	staffHours := make(map[string]float64)
	serviceTypeHours := make(map[string]float64)
	dailyHours := make(map[string]float64)
	weeklyHours := make(map[string]float64)
	monthlyHours := make(map[string]float64)
	totalHours := 0.0
	totalRevenue := 0.0

	for _, shift := range shifts {
		hours := shift.EndTime.Sub(shift.StartTime).Hours()
		revenue := hours * shift.HourlyRate
		totalHours += hours
		totalRevenue += revenue
		
		// Participant breakdown
		participantKey := shift.Participant.FirstName + " " + shift.Participant.LastName
		participantHours[participantKey] += hours
		
		// Staff breakdown
		staffKey := shift.Staff.FirstName + " " + shift.Staff.LastName
		staffHours[staffKey] += hours
		
		// Service type breakdown
		serviceTypeHours[shift.ServiceType] += hours
		
		// Time-based breakdowns
		dayKey := shift.StartTime.Format("2006-01-02")
		dailyHours[dayKey] += hours
		
		// Week breakdown (Monday = start of week)
		year, week := shift.StartTime.ISOWeek()
		weekKey := fmt.Sprintf("%d-W%02d", year, week)
		weeklyHours[weekKey] += hours
		
		// Monthly breakdown
		monthKey := shift.StartTime.Format("2006-01")
		monthlyHours[monthKey] += hours
	}

	// Calculate averages
	avgDailyHours := 0.0
	avgWeeklyHours := 0.0
	avgMonthlyHours := 0.0
	
	if len(dailyHours) > 0 {
		for _, hours := range dailyHours {
			avgDailyHours += hours
		}
		avgDailyHours /= float64(len(dailyHours))
	}
	
	if len(weeklyHours) > 0 {
		for _, hours := range weeklyHours {
			avgWeeklyHours += hours
		}
		avgWeeklyHours /= float64(len(weeklyHours))
	}
	
	if len(monthlyHours) > 0 {
		for _, hours := range monthlyHours {
			avgMonthlyHours += hours
		}
		avgMonthlyHours /= float64(len(monthlyHours))
	}

	// Staff efficiency metrics
	staffEfficiency := make(map[string]gin.H)
	for staffName, hours := range staffHours {
		var staffShifts int64
		h.DB.Model(&models.Shift{}).
			Joins("JOIN participants ON shifts.participant_id = participants.id").
			Joins("JOIN users ON shifts.staff_id = users.id").
			Where("participants.organization_id = ? AND shifts.status = ? AND CONCAT(users.first_name, ' ', users.last_name) = ?", 
				orgID, "completed", staffName).
			Count(&staffShifts)
			
		avgHoursPerShift := 0.0
		if staffShifts > 0 {
			avgHoursPerShift = hours / float64(staffShifts)
		}
		
		staffEfficiency[staffName] = gin.H{
			"total_hours": hours,
			"total_shifts": staffShifts,
			"avg_hours_per_shift": avgHoursPerShift,
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"summary": gin.H{
				"total_service_hours": totalHours,
				"total_revenue": totalRevenue,
				"shifts_included": len(shifts),
				"avg_daily_hours": avgDailyHours,
				"avg_weekly_hours": avgWeeklyHours,
				"avg_monthly_hours": avgMonthlyHours,
			},
			"breakdowns": gin.H{
				"participant_hours": participantHours,
				"staff_hours": staffHours,
				"service_type_hours": serviceTypeHours,
				"daily_hours": dailyHours,
				"weekly_hours": weeklyHours,
				"monthly_hours": monthlyHours,
			},
			"analytics": gin.H{
				"staff_efficiency": staffEfficiency,
				"peak_day": h.findPeakDay(dailyHours),
				"peak_week": h.findPeakWeek(weeklyHours),
				"peak_month": h.findPeakMonth(monthlyHours),
			},
			"report_generated": time.Now(),
		},
	})
}

func (h *Handler) GetParticipantReport(c *gin.Context) {
	orgID, exists := c.Get("org_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "UNAUTHORIZED",
				"message": "Organization not found in context",
			},
		})
		return
	}

	var participants []models.Participant
	h.DB.Where("organization_id = ?", orgID).Find(&participants)

	activeCount := 0
	inactiveCount := 0

	for _, p := range participants {
		if p.IsActive {
			activeCount++
		} else {
			inactiveCount++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"total_participants": len(participants),
			"active_participants": activeCount,
			"inactive_participants": inactiveCount,
			"report_generated": time.Now(),
		},
	})
}

func (h *Handler) GetStaffPerformance(c *gin.Context) {
	orgID, exists := c.Get("org_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "UNAUTHORIZED",
				"message": "Organization not found in context",
			},
		})
		return
	}

	// Get staff and their completed shifts
	var users []models.User
	h.DB.Where("organization_id = ? AND role IN (?)", orgID, []string{"care_worker", "support_coordinator"}).Find(&users)

	staffPerformance := make(map[string]interface{})

	for _, user := range users {
		var completedShifts int64
		h.DB.Model(&models.Shift{}).
			Joins("JOIN participants ON shifts.participant_id = participants.id").
			Where("shifts.staff_id = ? AND participants.organization_id = ? AND shifts.status = ?", 
				user.ID, orgID, "completed").
			Count(&completedShifts)

		staffPerformance[user.FirstName+" "+user.LastName] = gin.H{
			"completed_shifts": completedShifts,
			"role": user.Role,
			"is_active": user.IsActive,
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"staff_performance": staffPerformance,
			"total_staff": len(users),
			"report_generated": time.Now(),
		},
	})
}

func (h *Handler) ExportReport(c *gin.Context) {
	reportType := c.Param("type")
	format := c.DefaultQuery("format", "pdf")

	// For MVP, return mock export
	filename := reportType + "_report_" + time.Now().Format("2006_01_02") + "." + format
	
	c.Header("Content-Disposition", "attachment; filename="+filename)
	
	if format == "pdf" {
		c.Header("Content-Type", "application/pdf")
		c.String(http.StatusOK, "Mock PDF content for "+reportType+" report")
	} else if format == "csv" {
		c.Header("Content-Type", "text/csv")
		c.String(http.StatusOK, "Date,Type,Value\n2025-01-01,"+reportType+",100")
	} else {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "INVALID_FORMAT",
				"message": "Unsupported export format",
			},
		})
	}
}

func (h *Handler) GetReportTemplates(c *gin.Context) {
	templates := []gin.H{
		{
			"id": "revenue_monthly",
			"name": "Monthly Revenue Report",
			"description": "Monthly breakdown of revenue from completed shifts",
			"fields": []string{"month", "revenue", "hours", "shifts"},
		},
		{
			"id": "participant_summary",
			"name": "Participant Summary",
			"description": "Overview of all participants and their service usage",
			"fields": []string{"participant", "total_hours", "total_cost", "last_service"},
		},
		{
			"id": "staff_utilization",
			"name": "Staff Utilization Report",
			"description": "Staff performance and utilization metrics",
			"fields": []string{"staff_name", "shifts_completed", "hours_worked", "utilization_rate"},
		},
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    templates,
	})
}

// Helper methods for finding peak periods
func (h *Handler) findPeakDay(dailyHours map[string]float64) gin.H {
	maxHours := 0.0
	peakDay := ""
	
	for day, hours := range dailyHours {
		if hours > maxHours {
			maxHours = hours
			peakDay = day
		}
	}
	
	return gin.H{
		"date": peakDay,
		"hours": maxHours,
	}
}

func (h *Handler) findPeakWeek(weeklyHours map[string]float64) gin.H {
	maxHours := 0.0
	peakWeek := ""
	
	for week, hours := range weeklyHours {
		if hours > maxHours {
			maxHours = hours
			peakWeek = week
		}
	}
	
	return gin.H{
		"week": peakWeek,
		"hours": maxHours,
	}
}

func (h *Handler) findPeakMonth(monthlyHours map[string]float64) gin.H {
	maxHours := 0.0
	peakMonth := ""
	
	for month, hours := range monthlyHours {
		if hours > maxHours {
			maxHours = hours
			peakMonth = month
		}
	}
	
	return gin.H{
		"month": peakMonth,
		"hours": maxHours,
	}
}