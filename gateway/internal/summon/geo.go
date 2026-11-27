package summon

// goalOf is a verb's goal as the car derives it (Appendix A §3.5): goal_latitude and goal_longitude if both are
// present, else the phone position. cmd_forward and cmd_reverse carry no goal keys, so their goal is the phone.
func goalOf(m Message) (lat, lon float64) {
	if m.HasGoal {
		return m.GoalLat, m.GoalLon
	}
	return m.Lat, m.Lon
}
