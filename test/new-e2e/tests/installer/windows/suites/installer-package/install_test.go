// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

// Package installertests implements E2E tests for the Datadog installer package on Windows
package installertests

import (
	"testing"

	"github.com/DataDog/datadog-agent/test/new-e2e/pkg/e2e"
	awsHostWindows "github.com/DataDog/datadog-agent/test/new-e2e/pkg/provisioners/aws/host/windows"
	installerwindows "github.com/DataDog/datadog-agent/test/new-e2e/tests/installer/windows"
	"github.com/DataDog/datadog-agent/test/new-e2e/tests/installer/windows/consts"
	"github.com/DataDog/datadog-agent/test/new-e2e/tests/windows/common"
)

type testInstallerSuite struct {
	baseInstallerPackageSuite
}

// TestInstaller tests the installation of the Datadog installer on a system.
func TestInstaller(t *testing.T) {
	e2e.Run(t, &testInstallerSuite{}, e2e.WithProvisioner(awsHostWindows.ProvisionerNoAgentNoFakeIntake()))
}

// TestInstalls tests installing and uninstalling the latest version of the Datadog installer from the pipeline.
func (s *testInstallerSuite) TestInstalls() {
	s.Run("Fresh install", func() {
		s.freshInstall()
		s.Run("Start service with a configuration file", s.startServiceWithConfigFile)
		s.Run("Uninstall", func() {
			s.uninstall()
			s.Run("Install with existing configuration file", func() {
				s.installWithExistingConfigFile("with-config-install.log")
				s.Run("Repair", s.repair)
				s.Run("Purge", s.purge)
				s.Run("Install after purge", func() {
					s.installWithExistingConfigFile("after-purge-install.log")
				})
			})
		})
	})
}

func (s *testInstallerSuite) startServiceWithConfigFile() {
	// Arrange
	s.Env().RemoteHost.CopyFileFromFS(fixturesFS, "fixtures/sample_config", consts.ConfigPath)

	// Act
	s.Require().NoError(common.StartService(s.Env().RemoteHost, consts.ServiceName))

	// Assert
	s.Require().Host(s.Env().RemoteHost).HasARunningDatadogInstallerService()
	status, err := s.Installer().Status()
	s.Require().NoError(err)
	// with no packages installed just prints version
	// e.g. Datadog Installer v7.60.0-devel+git.56.86b2ae2
	s.Require().Contains(status, "Datadog Installer")
}

func (s *testInstallerSuite) uninstall() {
	// Arrange

	// Act
	s.Require().NoError(s.Installer().Uninstall())

	// Assert
	s.requireUninstalled()
	s.Require().Host(s.Env().RemoteHost).
		FileExists(consts.ConfigPath)
}

func (s *testInstallerSuite) installWithExistingConfigFile(logFilename string) {
	// Arrange

	// Act
	s.Require().NoError(s.Installer().Install(
		installerwindows.WithMSILogFile(logFilename),
	))

	// Assert
	s.requireInstalled()
	s.Require().Host(s.Env().RemoteHost).
		HasAService(consts.ServiceName).
		WithStatus("Running")
}

func (s *testInstallerSuite) repair() {
	// Arrange
	s.Require().NoError(common.StopService(s.Env().RemoteHost, consts.ServiceName))
	s.Require().NoError(s.Env().RemoteHost.Remove(consts.BinaryPath))

	// Act
	s.Require().NoError(s.Installer().Install(
		installerwindows.WithMSILogFile("repair.log"),
	))

	// Assert
	s.requireInstalled()
	s.Require().Host(s.Env().RemoteHost).
		HasAService(consts.ServiceName).
		WithStatus("Running")
}

func (s *testInstallerSuite) purge() {
	// Arrange

	// Act
	_, err := s.Installer().Purge()

	// Assert
	s.Assert().NoError(err)
	s.requireUninstalled()
	s.Require().Host(s.Env().RemoteHost).
		NoFileExists(`C:\ProgramData\Datadog Installer\packages\packages.db`)
}
