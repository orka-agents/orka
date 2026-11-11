package evals

// RolloutRefusal is a promote or rollback the controller declined, with a
// reason a promoter can act on. It is not a failure of the controller.
type RolloutRefusal struct {
	Reason string
}

func (r *RolloutRefusal) Error() string { return r.Reason }
