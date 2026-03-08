package beamsync

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// RunFirewallSetup attempts to run the firewall_setup.sh script using pkexec.
func RunFirewallSetup() error {
	fmt.Println("🛡️ Initiating Firewall Setup...")

	// Resolve binary location so we find the script relative to the executable,
	// rather than using any hardcoded developer path.
	exePath, err := os.Executable()
	if err != nil {
		exePath = "."
	}
	exeDir := filepath.Dir(exePath)

	potentialPaths := []string{
		filepath.Join(exeDir, "firewall_setup.sh"),
		filepath.Join(exeDir, "build", "linux", "firewall_setup.sh"),
		"firewall_setup.sh",
		"build/linux/firewall_setup.sh",
		"../build/linux/firewall_setup.sh",
	}

	var scriptPath string
	for _, path := range potentialPaths {
		if _, err := os.Stat(path); err == nil {
			absPath, err := filepath.Abs(path)
			if err == nil {
				scriptPath = absPath
				break
			}
		}
	}

	if scriptPath == "" {
		return fmt.Errorf("firewall_setup.sh not found in any known location")
	}

	fmt.Printf("🛡️ Found script at: %s\n", scriptPath)

	if err := os.Chmod(scriptPath, 0755); err != nil {
		fmt.Printf("⚠️ Warning: Could not chmod script: %v\n", err)
	}

	cmd := exec.Command("pkexec", scriptPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("firewall setup failed: %v\nOutput: %s", err, string(output))
	}

	fmt.Println("✅ Firewall Setup Output:\n", string(output))
	return nil
}
