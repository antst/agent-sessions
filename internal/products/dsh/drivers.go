package dsh

import "fmt"

type DriverConfig struct {
	Lane   LaneConfig
	Doctor DoctorConfig
}

type Drivers struct {
	Lane   *LaneDriver
	Doctor *DoctorProbe
}

func NewDrivers(config DriverConfig) (Drivers, error) {
	doctor, err := NewDoctorProbe(config.Doctor)
	if err != nil {
		return Drivers{}, err
	}
	lane, err := NewLaneDriver(config.Lane)
	if err != nil {
		return Drivers{}, err
	}
	if lane.config.Executable != doctor.config.Executable {
		return Drivers{}, fmt.Errorf("DSH lane and doctor must share one executable")
	}
	return Drivers{Lane: lane, Doctor: doctor}, nil
}
