package main

import "fmt"

func (s *Service) beginInstall(serverID int, reinstall bool) error {
	if serverID <= 0 {
		return fmt.Errorf("invalid server id")
	}
	label := "install"
	if reinstall {
		label = "reinstall"
	}

	if existing, loaded := s.installState.LoadOrStore(serverID, label); loaded {
		existingLabel, _ := existing.(string)
		if existingLabel == "" {
			existingLabel = "install"
		}
		return fmt.Errorf("another %s is already running for this server", existingLabel)
	}

	s.powerStateMu.Lock()
	defer s.powerStateMu.Unlock()
	if action := s.powerState[serverID]; action != "" {
		s.installState.Delete(serverID)
		return fmt.Errorf("cannot execute server %s while another power action is running (%s)", label, action)
	}
	return nil
}

func (s *Service) finishInstall(serverID int) {
	if serverID <= 0 {
		return
	}
	s.installState.Delete(serverID)
}

func (s *Service) isInstalling(serverID int) bool {
	if serverID <= 0 {
		return false
	}
	_, ok := s.installState.Load(serverID)
	return ok
}

func (s *Service) beginPowerAction(serverID int, action string) error {
	if serverID <= 0 {
		return fmt.Errorf("invalid server id")
	}
	if s.isInstalling(serverID) {
		return fmt.Errorf("cannot execute power action while server install or reinstall is running")
	}

	s.powerStateMu.Lock()
	defer s.powerStateMu.Unlock()
	if existing := s.powerState[serverID]; existing != "" {
		return fmt.Errorf("another power action is already running (%s)", existing)
	}
	s.powerState[serverID] = action
	return nil
}

func (s *Service) finishPowerAction(serverID int) {
	if serverID <= 0 {
		return
	}
	s.powerStateMu.Lock()
	delete(s.powerState, serverID)
	s.powerStateMu.Unlock()
}

func (s *Service) isPowerActionRunning(serverID int) bool {
	if serverID <= 0 {
		return false
	}
	s.powerStateMu.Lock()
	defer s.powerStateMu.Unlock()
	return s.powerState[serverID] != ""
}
