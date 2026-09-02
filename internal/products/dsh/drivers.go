package dsh

import "errors"

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
	if config.Lane.TupleVerifier == nil {
		config.Lane.TupleVerifier = doctor
	}
	lane, err := NewLaneDriver(config.Lane)
	if err != nil {
		return Drivers{}, err
	}
	if lane.config.Executable != doctor.config.Executable || lane.config.DSHHome != doctor.config.DSHHome {
		return Drivers{}, errors.New("DSH lane and doctor must share one executable and managed home")
	}
	return Drivers{Lane: lane, Doctor: doctor}, nil
}
