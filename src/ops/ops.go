package ops

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"text/template"

	"io/ioutil"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/openshift/assisted-installer/src/config"
	"github.com/openshift/assisted-installer/src/inventory_client"
	"github.com/openshift/assisted-installer/src/utils"
)

const (
	coreosInstallerExecutable       = "coreos-installer"
	dryRunCoreosInstallerExecutable = "dry-installer"
)

//go:generate mockgen -source=ops.go -package=ops -destination=mock_ops.go
type Ops interface {
	ExecPrivilegeCommand(liveLogger io.Writer, command string, args ...string) (string, error)
	ExecCommand(liveLogger io.Writer, command string, args ...string) (string, error)
	Mkdir(dirName string) error
	WriteImageToDisk(ignitionPath string, device string, progressReporter inventory_client.InventoryClient, extra []string) error
	Reboot() error
	SetBootOrder(device string) error
	ExtractFromIgnition(ignitionPath string, fileToExtract string) error
	SystemctlAction(action string, args ...string) error
	PrepareController() error
	GetVGByPV(pvName string) (string, error)
	RemoveVG(vgName string) error
	RemoveLV(lvName, vgName string) error
	RemovePV(pvName string) error
	Wipefs(device string) error
	GetMCSLogs() (string, error)
	UploadInstallationLogs(isBootstrap bool) (string, error)
	ReloadHostFile(filepath string) error
	CreateOpenshiftSshManifest(filePath, template, sshPubKeyPath string) error
	GetMustGatherLogs(workDir, kubeconfigPath string, images ...string) (string, error)
	CreateRandomHostname(hostname string) error
	GetHostname() (string, error)
	EvaluateDiskSymlink(string) string
	FormatDisk(string) error
	CreateManifests(string, []byte) error
	DryRebootHappened(markerPath string) bool
}

const (
	controllerDeployFolder         = "/assisted-installer-controller/deploy"
	manifestsFolder                = "/opt/openshift/manifests"
	renderedControllerCm           = "assisted-installer-controller-cm.yaml"
	controllerDeployCmTemplate     = "assisted-installer-controller-cm.yaml.template"
	renderedControllerPod          = "assisted-installer-controller-pod.yaml"
	controllerDeployPodTemplate    = "assisted-installer-controller-pod.yaml.template"
	renderedControllerSecret       = "assisted-installer-controller-secret.yaml"
	controllerDeploySecretTemplate = "assisted-installer-controller-secret.yaml.template"
)

type ops struct {
	log       *logrus.Logger
	logWriter *utils.LogWriter
	cmdEnv    []string
}

// NewOps return a new ops interface
func NewOps(logger *logrus.Logger, proxySet bool) Ops {
	cmdEnv := os.Environ()
	if proxySet && (config.GlobalConfig.HTTPProxy != "" || config.GlobalConfig.HTTPSProxy != "") {
		if config.GlobalConfig.HTTPProxy != "" {
			cmdEnv = append(cmdEnv, fmt.Sprintf("HTTP_PROXY=%s", config.GlobalConfig.HTTPProxy))
		}
		if config.GlobalConfig.HTTPSProxy != "" {
			cmdEnv = append(cmdEnv, fmt.Sprintf("HTTPS_PROXY=%s", config.GlobalConfig.HTTPSProxy))
		}
		if config.GlobalConfig.NoProxy != "" {
			cmdEnv = append(cmdEnv, fmt.Sprintf("NO_PROXY=%s", config.GlobalConfig.NoProxy))
		}
	}
	return &ops{logger, utils.NewLogWriter(logger), cmdEnv}
}

// ExecPrivilegeCommand execute a command in the host environment via nsenter

func (o *ops) ExecPrivilegeCommand(liveLogger io.Writer, command string, args ...string) (string, error) {
	// nsenter is used here to launch processes inside the container in a way that makes said processes feel
	// and behave as if they're running on the host directly rather than inside the container
	commandBase := "nsenter"

	arguments := []string{
		"--target", "1",
		// Entering the cgroup namespace is not required for podman on CoreOS (where the
		// agent typically runs), but it's needed on some Fedora versions and
		// some other systemd based systems. Those systems are used to run dry-mode
		// agents for load testing. If this flag is not used, Podman will sometimes
		// have trouble creating a systemd cgroup slice for new containers.
		"--cgroup",
		// The mount namespace is required for podman to access the host's container
		// storage
		"--mount",
		// TODO: Document why we need the IPC namespace
		"--ipc",
		"--pid",
		"--",
		command,
	}

	arguments = append(arguments, args...)
	return o.ExecCommand(liveLogger, commandBase, arguments...)
}

type ExecCommandError struct {
	Command    string
	Args       []string
	Env        []string
	ExitErr    error
	Output     string
	WaitStatus int
}

func removePullSecret(s []string) []string {
	if config.GlobalConfig.PullSecretToken == "" {
		return s
	}

	return strings.Split(strings.ReplaceAll(strings.Join(s, " "), config.GlobalConfig.PullSecretToken, "<SECRET>"), " ")
}

func (e *ExecCommandError) Error() string {
	lastOutput := e.Output
	if len(e.Output) > 200 {
		lastOutput = "... " + e.Output[len(e.Output)-200:]
	}
	return fmt.Sprintf("failed executing %s %v, Error %s, LastOutput \"%s\"", e.Command, removePullSecret(e.Args), e.ExitErr, lastOutput)
}

func (e *ExecCommandError) DetailedError() string {
	return fmt.Sprintf("failed executing %s %v, env vars %v, error %s, waitStatus %d, Output \"%s\"", e.Command, removePullSecret(e.Args), removePullSecret(e.Env), e.ExitErr, e.WaitStatus, e.Output)
}

// ExecCommand executes command.
func (o *ops) ExecCommand(liveLogger io.Writer, command string, args ...string) (string, error) {

	var stdoutBuf bytes.Buffer
	cmd := exec.Command(command, args...)
	if liveLogger != nil {
		cmd.Stdout = io.MultiWriter(liveLogger, &stdoutBuf)
		cmd.Stderr = io.MultiWriter(liveLogger, &stdoutBuf)
	} else {
		cmd.Stdout = &stdoutBuf
		cmd.Stderr = &stdoutBuf
	}
	cmd.Env = o.cmdEnv
	err := cmd.Run()
	output := strings.TrimSpace(stdoutBuf.String())
	if err != nil {

		// Get all lines from Error message
		errorIndex := strings.Index(output, "Error")
		// if Error not found return all output
		if errorIndex > -1 {
			output = output[errorIndex:]
		}

		execErr := &ExecCommandError{
			Command: command,
			Args:    args,
			Env:     cmd.Env,
			ExitErr: err,
			Output:  output,
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				execErr.WaitStatus = status.ExitStatus()
			}
		}
		if liveLogger != nil {
			//If the caller didn't provide liveLogger the log isn't interesting and might spam
			o.log.Info(execErr.DetailedError())
		}
		return output, execErr
	}
	o.log.Debug("Command executed:", " command", command, " arguments", removePullSecret(args), "env vars", removePullSecret(cmd.Env), "output", output)
	return output, err
}

func (o *ops) Mkdir(dirName string) error {
	o.log.Infof("Creating directory: %s", dirName)
	_, err := o.ExecPrivilegeCommand(o.logWriter, "mkdir", "-p", dirName)
	return err
}

func (o *ops) SystemctlAction(action string, args ...string) error {
	if config.GlobalDryRunConfig.DryRunEnabled {
		return nil
	}

	o.log.Infof("Running systemctl %s %s", action, args)
	_, err := o.ExecPrivilegeCommand(o.logWriter, "systemctl", append([]string{action}, args...)...)
	if err != nil {
		o.log.Errorf("Failed executing systemctl %s %s", action, args)
	}
	return errors.Wrapf(err, "Failed executing systemctl %s %s", action, args)
}

func (o *ops) WriteImageToDisk(ignitionPath string, device string, progressReporter inventory_client.InventoryClient, extraArgs []string) error {
	allArgs := installerArgs(ignitionPath, device, extraArgs)
	o.log.Infof("Writing image and ignition to disk with arguments: %v", allArgs)

	installerExecutable := coreosInstallerExecutable
	if config.GlobalDryRunConfig.DryRunEnabled {
		// In dry run, we use an executable called dry-installer rather than coreos-installer.
		// This executable is expected to pretend to be doing coreos-installer stuff and print fake
		// progress. It's up to the dry-mode user to make sure such executable is available in PATH
		installerExecutable = dryRunCoreosInstallerExecutable
	}

	_, err := o.ExecPrivilegeCommand(NewCoreosInstallerLogWriter(o.log, progressReporter, config.GlobalConfig.InfraEnvID, config.GlobalConfig.HostID),
		installerExecutable, allArgs...)
	return err
}

func (o *ops) EvaluateDiskSymlink(device string) string {
	// Overcome https://github.com/coreos/coreos-installer/issues/512 bug.
	// coreos-installer has a bug where when a disk has busy partitions, it will
	// print a confusing error message if that disk doesn't have a `/dev/*` style path.
	// The service may give us paths that don't have the `/dev/*` path format but instead
	// are symlinks to the actual `/dev/*` path. e.g. `/dev/disk/by-id/wwn-*`.
	// To fix the bug we simply resolve the symlink and pass the resolved link to coreos-installer.
	linkTarget, err := filepath.EvalSymlinks(device)
	if err != nil {
		o.log.Warnf("Failed to filepath.EvalSymlinks(%s): %s. Continuing with %s anyway.",
			device, err.Error(), device)
	} else {
		o.log.Infof("Resolving installation device %s symlink to %s ", device, linkTarget)
		device = linkTarget
	}
	return device
}

func (o *ops) FormatDisk(disk string) error {
	if config.GlobalDryRunConfig.DryRunEnabled {
		return nil
	}

	o.log.Infof("Formatting disk %s", disk)
	_, err := o.ExecPrivilegeCommand(o.logWriter, "dd", "if=/dev/zero", fmt.Sprintf("of=%s", disk), "bs=512", "count=1")
	if err != nil {
		o.log.Errorf("Failed to format disk %s, err: %s", disk, err)
		return err
	}
	return nil
}

func installerArgs(ignitionPath string, device string, extra []string) []string {
	allArgs := []string{"install", "--insecure", "-i", ignitionPath}
	if extra != nil {
		allArgs = append(allArgs, extra...)
	}
	return append(allArgs, device)
}

func (o *ops) Reboot() error {
	if config.GlobalDryRunConfig.DryRunEnabled {
		_, err := o.ExecPrivilegeCommand(o.logWriter, "touch", config.GlobalDryRunConfig.FakeRebootMarkerPath)
		return errors.Wrap(err, "failed to touch fake reboot marker")
	}

	o.log.Info("Rebooting node")
	_, err := o.ExecPrivilegeCommand(o.logWriter, "shutdown", "-r", "+1", "'Installation completed, server is going to reboot.'")
	if err != nil {
		o.log.Errorf("Failed to reboot node, err: %s", err)
		return err
	}
	return nil
}

func (o *ops) SetBootOrder(device string) error {
	if config.GlobalDryRunConfig.DryRunEnabled {
		return nil
	}

	_, err := o.ExecPrivilegeCommand(nil, "test", "-d", "/sys/firmware/efi")
	if err != nil {
		o.log.Info("setting the boot order on BIOS systems is not supported. Skipping...")
		return nil
	}

	o.log.Info("Setting efibootmgr to boot from disk")

	// efi-system is installed onto partition 2
	_, err = o.ExecPrivilegeCommand(o.logWriter, "efibootmgr", "-v", "-d", device, "-p", "2", "-c", "-L", "Red Hat Enterprise Linux", "-l", o.getEfiFilePath())
	if err != nil {
		o.log.Errorf("Failed to set efibootmgr to boot from disk %s, err: %s", device, err)
		return err
	}
	return nil
}

func (o *ops) getEfiFilePath() string {
	var efiFileName string
	switch runtime.GOARCH {
	case "arm64":
		efiFileName = "shimaa64.efi"
	default:
		efiFileName = "shimx64.efi"
	}
	o.log.Infof("Using EFI file '%s' for GOARCH '%s'", efiFileName, runtime.GOARCH)
	return fmt.Sprintf("\\EFI\\redhat\\%s", efiFileName)
}

func (o *ops) ExtractFromIgnition(ignitionPath string, fileToExtract string) error {
	if config.GlobalDryRunConfig.DryRunEnabled {
		return nil
	}

	o.log.Infof("Getting data from %s", ignitionPath)
	ignitionData, err := ioutil.ReadFile(ignitionPath)
	if err != nil {
		o.log.Errorf("Error occurred while trying to read %s : %e", ignitionPath, err)
		return err
	}
	extractedContent, err := utils.GetFileContentFromIgnition(ignitionData, fileToExtract)
	if err != nil {
		o.log.Error("Failed to parse ignition")
		return err
	}

	tmpFile := "/opt/extracted_from_ignition.json"
	o.log.Infof("Writing extracted content to tmp file %s", tmpFile)
	// #nosec
	err = ioutil.WriteFile(tmpFile, extractedContent, 0644)
	if err != nil {
		o.log.Errorf("Error occurred while writing extracted content to %s", tmpFile)
		return err
	}

	o.log.Infof("Moving %s to %s", tmpFile, fileToExtract)
	dir := filepath.Dir(fileToExtract)
	_, err = o.ExecPrivilegeCommand(o.logWriter, "mkdir", "-p", filepath.Dir(fileToExtract))
	if err != nil {
		o.log.Errorf("Failed to create directory %s ", dir)
		return err
	}
	_, err = o.ExecPrivilegeCommand(o.logWriter, "mv", tmpFile, fileToExtract)
	if err != nil {
		o.log.Errorf("Error occurred while moving %s to %s", tmpFile, fileToExtract)
		return err
	}
	return nil
}

func (o *ops) PrepareController() error {

	if err := o.renderControllerCm(); err != nil {
		return err
	}

	if err := o.renderControllerSecret(); err != nil {
		return err
	}

	if err := o.renderControllerPod(); err != nil {
		return err
	}

	// Copy deploy files to manifestsFolder
	files, err := utils.FindFiles(controllerDeployFolder, utils.W_FILEONLY, "*.yaml")
	if err != nil {
		o.log.Errorf("Error occurred while trying to get list of files from %s : %e", controllerDeployFolder, err)
		return err
	}
	for _, file := range files {
		err := utils.CopyFile(file, filepath.Join(manifestsFolder, filepath.Base(file)))
		if err != nil {
			o.log.Errorf("Failed to copy %s to %s. error :%e", file, manifestsFolder, err)
			return err
		}
	}
	return nil
}

func (o *ops) renderControllerCm() error {
	var params = map[string]interface{}{
		"InventoryUrl":         config.GlobalConfig.URL,
		"ClusterId":            config.GlobalConfig.ClusterID,
		"SkipCertVerification": strconv.FormatBool(config.GlobalConfig.SkipCertVerification),
		"CACertPath":           config.GlobalConfig.CACertPath,
		"HaMode":               config.GlobalConfig.HighAvailabilityMode,
		"CheckCVO":             config.GlobalConfig.CheckClusterVersion,
		"MustGatherImage":      config.GlobalConfig.MustGatherImage,
	}

	return o.renderDeploymentFiles(filepath.Join(controllerDeployFolder, controllerDeployCmTemplate),
		params, renderedControllerCm)
}

func (o *ops) renderControllerSecret() error {
	var params = map[string]interface{}{
		"PullSecretToken": config.GlobalConfig.PullSecretToken,
	}

	return o.renderDeploymentFiles(filepath.Join(controllerDeployFolder, controllerDeploySecretTemplate),
		params, renderedControllerSecret)
}

func (o *ops) renderControllerPod() error {
	var params = map[string]interface{}{
		"ControllerImage":  config.GlobalConfig.ControllerImage,
		"CACertPath":       config.GlobalConfig.CACertPath,
		"OpenshiftVersion": config.GlobalConfig.OpenshiftVersion,
	}

	if config.GlobalConfig.ServiceIPs != "" {
		params["ServiceIPs"] = strings.Split(config.GlobalConfig.ServiceIPs, ",")
	}

	return o.renderDeploymentFiles(filepath.Join(controllerDeployFolder, controllerDeployPodTemplate),
		params, renderedControllerPod)
}

func (o *ops) renderDeploymentFiles(srcTemplate string, params map[string]interface{}, dest string) error {
	templateData, err := ioutil.ReadFile(srcTemplate)
	if err != nil {
		o.log.Errorf("Error occurred while trying to read %s : %e", srcTemplate, err)
		return err
	}
	o.log.Infof("Filling template file %s", srcTemplate)
	tmpl := template.Must(template.New("assisted-controller").Parse(string(templateData)))
	var buf bytes.Buffer
	if err = tmpl.Execute(&buf, params); err != nil {
		o.log.Errorf("Failed to render controller template: %e", err)
		return err
	}

	if err = o.Mkdir(manifestsFolder); err != nil {
		o.log.Errorf("Failed to create manifests dir: %e", err)
		return err
	}

	renderedControllerYaml := filepath.Join(manifestsFolder, dest)
	o.log.Infof("Writing rendered data to %s", renderedControllerYaml)
	// #nosec
	if err = ioutil.WriteFile(renderedControllerYaml, buf.Bytes(), 0644); err != nil {
		o.log.Errorf("Error occurred while trying to write rendered data to %s : %e", renderedControllerYaml, err)
		return err
	}
	return nil
}

func (o *ops) GetVGByPV(pvName string) (string, error) {
	output, err := o.ExecPrivilegeCommand(o.logWriter, "vgs", "--noheadings", "-o", "vg_name,pv_name")
	if err != nil {
		o.log.Errorf("Failed to list VGs in the system")
		return "", err
	}

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		res := strings.Fields(line)
		if len(res) < 2 {
			continue
		}

		if strings.Contains(res[1], pvName) {
			return res[0], nil
		}
	}
	return "", nil
}

func (o *ops) RemoveVG(vgName string) error {
	output, err := o.ExecPrivilegeCommand(o.logWriter, "vgremove", vgName, "-y")
	if err != nil {
		o.log.Errorf("Failed to remove VG %s, output %s, error %s", vgName, output, err)
	}
	return err
}

func (o *ops) RemoveLV(lvName, vgName string) error {
	output, err := o.ExecPrivilegeCommand(o.logWriter, "lvremove", fmt.Sprintf("/dev/%s/%s", vgName, lvName), "-y")
	if err != nil {
		o.log.Errorf("Failed to remove LVM %s, output %s, error %s", fmt.Sprintf("/dev/%s/%s", vgName, lvName), output, err)
	}
	return err
}

func (o *ops) RemovePV(pvName string) error {
	output, err := o.ExecPrivilegeCommand(o.logWriter, "pvremove", pvName, "-y", "-ff")
	if err != nil {
		o.log.Errorf("Failed to remove PV %s, output %s, error %s", pvName, output, err)
	}
	return err
}

func (o *ops) Wipefs(device string) error {
	output, err := o.ExecPrivilegeCommand(o.logWriter, "wipefs", "-a", device)
	if err != nil {
		o.log.Errorf("Failed to wipefs device %s, output %s, error %s", device, output, err)
	}
	return err
}

func (o *ops) GetMCSLogs() (string, error) {
	if config.GlobalDryRunConfig.DryRunEnabled {
		mcsLogs := ""
		for _, clusterHost := range config.GlobalDryRunConfig.ParsedClusterHosts {
			// Add IP access log for each IP, this is how the installer determines which node has downloaded the ignition
			if !o.DryRebootHappened(clusterHost.RebootMarkerPath) {
				// Host didn't even reboot yet, don't pretend it fetched the ignition
				continue
			}
			mcsLogs += fmt.Sprintf("%s.(Ignition)\n", clusterHost.Ip)
		}
		return mcsLogs, nil
	}

	files, err := utils.FindFiles("/var/log/containers/", utils.W_FILEONLY, "*machine-config-server*.log")
	if err != nil {
		o.log.WithError(err).Errorf("Error occurred while trying to get list of files from %s", "/var/log/containers/")
		return "", err
	}
	if len(files) < 1 {
		o.log.Warnf("MCS log file not found")
		return "", err
	}
	// There is theoretical option in case of static pod restart that there can be more than one file
	// we never saw it and it was decided not to handle it here
	logs, err := ioutil.ReadFile(files[0])
	if err != nil {
		o.log.Errorf("Error occurred while trying to read %s : %e", files[0], err)
		return "", err
	}

	return string(logs), nil
}

// This function actually runs container that imeplements logs_sender command
// Any change to the assisted-service API that is used by the logs_sender command
// ( for example UploadLogs), must be reflected here (input parameters, etc'),
// if needed
func (o *ops) UploadInstallationLogs(isBootstrap bool) (string, error) {
	command := "podman"
	args := []string{"run", "--rm", "--privileged", "--net=host", "--pid=host", "-v", "/run/systemd/journal/socket:/run/systemd/journal/socket",
		"-v", "/var/log:/var/log", config.GlobalConfig.AgentImage, "logs_sender",
		"-cluster-id", config.GlobalConfig.ClusterID, "-url", config.GlobalConfig.URL,
		"-host-id", config.GlobalConfig.HostID, "-infra-env-id", config.GlobalConfig.InfraEnvID,
		"-pull-secret-token", config.GlobalConfig.PullSecretToken,
		fmt.Sprintf("-insecure=%s", strconv.FormatBool(config.GlobalConfig.SkipCertVerification)),
		fmt.Sprintf("-bootstrap=%s", strconv.FormatBool(isBootstrap)),
	}

	if config.GlobalConfig.CACertPath != "" {
		args = append(args, fmt.Sprintf("-cacert=%s", config.GlobalConfig.CACertPath))
	}
	return o.ExecPrivilegeCommand(o.logWriter, command, args...)
}

// Sometimes we will need to reload container files from host
// For example /etc/resolv.conf, it can't be changed with Z flag but is updated by bootkube.sh
// and we need this update for dns resolve of kubeapi
func (o *ops) ReloadHostFile(filepath string) error {
	if config.GlobalDryRunConfig.DryRunEnabled {
		return nil
	}

	o.log.Infof("Reloading %s", filepath)
	output, err := o.ExecPrivilegeCommand(o.logWriter, "cat", filepath)
	if err != nil {
		o.log.Errorf("Failed to read %s on the host", filepath)
		return err
	}
	f, err := os.OpenFile(filepath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	defer func() {
		_ = f.Close()
	}()
	if err != nil {
		o.log.Errorf("Failed to open local %s", filepath)
		return err
	}
	_, err = f.WriteString(output)
	if err != nil {
		o.log.Errorf("Failed to write host %s data to local", filepath)
		return err
	}
	return nil
}

func (o *ops) CreateOpenshiftSshManifest(filePath, tmpl, sshPubKeyPath string) error {
	if config.GlobalDryRunConfig.DryRunEnabled {
		return nil
	}

	o.log.Info("Create an openshift manifets for SSH public key")
	sshPublicKey, err := o.ExecPrivilegeCommand(o.logWriter, "cat", sshPubKeyPath)
	if err != nil {
		o.log.WithError(err).Errorf("Failed to read SSH pub key from %s", sshPubKeyPath)
		return err
	}
	f, err := os.Create(filePath)
	if err != nil {
		o.log.WithError(err).Errorf("Failed to create %s", filePath)
		return err
	}
	defer f.Close()
	t := template.Must(template.New("openshift SSH manifest").Parse(tmpl))
	sshConfig := struct {
		SshPubKey string
	}{sshPublicKey}
	if err := t.Execute(f, sshConfig); err != nil {
		o.log.WithError(err).Error("Failed to execute template")
		return err
	}
	return nil
}

func (o *ops) GetMustGatherLogs(workDir, kubeconfigPath string, images ...string) (string, error) {
	//invoke oc adm must-gather command in the working directory
	var imageOption string = ""
	for _, img := range images {
		imageOption = imageOption + fmt.Sprintf(" --image=%s", img)
	}

	command := fmt.Sprintf("cd %s && oc --kubeconfig=%s adm must-gather%s", workDir, kubeconfigPath, imageOption)
	output, err := o.ExecCommand(o.logWriter, "bash", "-c", command)
	if err != nil {
		return "", err
	}
	o.log.Info(output)

	//find the directory of logs which is the output of the command
	//this is a temp directory so we have to find it by its prefix
	files, err := utils.FindFiles(workDir, utils.W_DIRONLY, "must-gather*")
	if err != nil {
		o.log.WithError(err).Errorf("Failed to read must-gather working dir %s\n", workDir)
		return "", err
	}

	if len(files) == 0 {
		lerr := fmt.Errorf("Failed to find must-gather output")
		o.log.Errorf(lerr.Error())
		return "", lerr
	}
	logsDir := filepath.Base(files[0])

	//tar the log directory and return the path to the tarball
	tarName := "must-gather.tar.gz"
	command = fmt.Sprintf("cd %s && tar zcf %s %s", workDir, tarName, logsDir)
	_, err = o.ExecCommand(o.logWriter, "bash", "-c", command)
	if err != nil {
		o.log.WithError(err).Errorf("Failed to tar must-gather logs\n")
		return "", err
	}
	return path.Join(workDir, tarName), nil
}

func (o *ops) CreateRandomHostname(hostname string) error {
	command := fmt.Sprintf("echo %s > /etc/hostname", hostname)
	o.log.Infof("create random hostname with command %s", command)
	_, err := o.ExecPrivilegeCommand(o.logWriter, "bash", "-c", command)
	return err
}

func (o *ops) GetHostname() (string, error) {
	return os.Hostname()
}

func (o *ops) CreateManifests(kubeconfig string, content []byte) error {
	// Create temp file, where we store the content to be create by oc command:
	file, err := ioutil.TempFile("", "operator-manifest")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())

	// Write the content to the temporary file:
	if err = ioutil.WriteFile(file.Name(), content, 0644); err != nil {
		return err
	}

	// Run oc command that creates the custom manifest:
	command := fmt.Sprintf("oc --kubeconfig=%s apply -f %s", kubeconfig, file.Name())
	output, err := o.ExecCommand(o.logWriter, "bash", "-c", command)
	if err != nil {
		return err
	}
	o.log.Infof("Applying custom manifest file %s succeed %s", file.Name(), output)

	return nil
}

// DryRebootHappened checks if a reboot happened according to a particular reboot marker path
// The dry run installer creates this file on "Reboot" (instead of actually rebooting)
// We use this function to check whether the given node in the cluster have already rebooted
func (o *ops) DryRebootHappened(markerPath string) bool {
	_, err := o.ExecPrivilegeCommand(nil, "stat", markerPath)

	return err == nil
}
