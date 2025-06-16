package tektonconfig

import (
  "fmt"
  "os"
  "strings"

  v1alpha1 "github.com/tektoncd/operator/pkg/apis/operator/v1alpha1"
)

/*func ProxyTransformer(tc *v1alpha1.TektonConfig) {
        http := os.Getenv("HTTP_PROXY")
        https := os.Getenv("HTTPS_PROXY")
        no := os.Getenv("NO_PROXY")
        if http == "" && https == "" && no == "" {
                return
        }

        // Build the proxy block
        block := []string{"env:"}
        if http != "" {
                block = append(block,
                        fmt.Sprintf("  - name: HTTP_PROXY\n    value: %q", http))
        }
        if https != "" {
                block = append(block,
                        fmt.Sprintf("  - name: HTTPS_PROXY\n    value: %q", https))
        }
        if no != "" {
                block = append(block,
                        fmt.Sprintf("  - name: NO_PROXY\n    value: %q", no))
        }
        proxyYAML := strings.Join(block, "\n")

        // If none existed, just set it
        if strings.TrimSpace(tc.Spec.Pipeline.DefaultPodTemplate) == "" {
                tc.Spec.Pipeline.DefaultPodTemplate = proxyYAML
                return
        }

        // Otherwise merge: remove old proxy lines and append
        lines := strings.Split(tc.Spec.Pipeline.DefaultPodTemplate, "\n")
        var out []string
        for _, l := range lines {
                if strings.Contains(l, "name: HTTP_PROXY") ||
                   strings.Contains(l, "name: HTTPS_PROXY") ||
                   strings.Contains(l, "name: NO_PROXY") {
                        continue
                }
                out = append(out, l)
        }
        out = append(out, proxyYAML)
        tc.Spec.Pipeline.DefaultPodTemplate = strings.Join(out, "\n")
}*/

// ProxyTransformer sets tc.Spec.Pipeline.DefaultPodTemplate to include HTTP_PROXY/HTTPS_PROXY/NO_PROXY.
// It only appends the proxy block if it is not already present exactly at the end.
/*func ProxyTransformer(tc *v1alpha1.TektonConfig) {
	http := os.Getenv("HTTP_PROXY")
	https := os.Getenv("HTTPS_PROXY")
	no := os.Getenv("NO_PROXY")
	if http == "" && https == "" && no == "" {
		// Nothing to inject
		return
	}

	// 1. Build the proxy block (exactly once).
	//
	//    We include a leading newline so that when we append to existing text, it
	//    always starts on a new line. If DefaultPodTemplate is empty, the newline is
	//    harmless because TrimSpace later will still detect an empty string.
	//
	blockLines := []string{
		"",
		"env:",
	}
	if http != "" {
		blockLines = append(blockLines,
			fmt.Sprintf("  - name: HTTP_PROXY"),
			fmt.Sprintf("    value: %q", http),
		)
	}
	if https != "" {
		blockLines = append(blockLines,
			fmt.Sprintf("  - name: HTTPS_PROXY"),
			fmt.Sprintf("    value: %q", https),
		)
	}
	if no != "" {
		blockLines = append(blockLines,
			fmt.Sprintf("  - name: NO_PROXY"),
			fmt.Sprintf("    value: %q", no),
		)
	}
	// Join with "\n" so that indentation is exactly two spaces before "- name".
	proxyBlock := strings.Join(blockLines, "\n")

	// 2. If DefaultPodTemplate already ends exactly with our proxyBlock, do nothing.
	//
	current := tc.Spec.Pipeline.DefaultPodTemplate
	if strings.HasSuffix(current, proxyBlock) {
		// Already injected exactly; skip.
		return
	}

	// 3. Otherwise, strip out any old proxy lines (any line containing name: HTTP_PROXY etc.)
	//    and then append the canonical proxyBlock once.
	//
	if strings.TrimSpace(current) == "" {
		// No existing content → just set it to proxyBlock (but TrimSpace removes the leading newline)
		tc.Spec.Pipeline.DefaultPodTemplate = strings.TrimPrefix(proxyBlock, "\n")
		return
	}

	// Split into lines, filter out any old proxy entries
	lines := strings.Split(current, "\n")
	var filtered []string
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if strings.Contains(l, "name: HTTP_PROXY") ||
			strings.Contains(l, "name: HTTPS_PROXY") ||
			strings.Contains(l, "name: NO_PROXY") {
			// Skip this line and its following " value:" line
			i++ // skip the next line that holds `value: "…"`
			continue
		}
		filtered = append(filtered, l)
	}

	// Re‐join filtered lines, then append the proxy block (with its leading newline).
	tc.Spec.Pipeline.DefaultPodTemplate = strings.Join(filtered, "\n") + proxyBlock
}*/

// ProxyTransformer builds a YAML fragment for default-pod-template that looks like:
// 
//   env:
//     - name: HTTP_PROXY
//       value: "http://..."
//     - name: HTTPS_PROXY
//       value: "http://..."
//     - name: NO_PROXY
//       value: "10.96.0.1,*.cluster.local,*.svc"
// 
// and injects it into tc.Spec.Pipeline.DefaultPodTemplate only once. Subsequent calls
// detect the exact same block and do nothing.
//
// The key differences from the previous version are:
//  1. We do NOT prepend a leading newline to the block.
//  2. We build exactly one "env:" line, not two.
//  3. We use strings.HasSuffix to detect when the block is already present, preventing duplicates.
/*func ProxyTransformer(tc *v1alpha1.TektonConfig) {
	http := os.Getenv("HTTP_PROXY")
	https := os.Getenv("HTTPS_PROXY")
	no := os.Getenv("NO_PROXY")

	// If none of the three proxy envs are set, do nothing.
	if http == "" && https == "" && no == "" {
		return
	}

	// 1. Build the canonical proxyBlock (no leading newline).
	//    Each line is indented exactly as you want under "default-pod-template: |".
	blockLines := []string{
		"env:",
	}
	if http != "" {
		blockLines = append(blockLines,
			fmt.Sprintf("  - name: HTTP_PROXY"),
			fmt.Sprintf("    value: %q", http),
		)
	}
	if https != "" {
		blockLines = append(blockLines,
			fmt.Sprintf("  - name: HTTPS_PROXY"),
			fmt.Sprintf("    value: %q", https),
		)
	}
	if no != "" {
		blockLines = append(blockLines,
			fmt.Sprintf("  - name: NO_PROXY"),
			fmt.Sprintf("    value: %q", no),
		)
	}
	// Join with "\n" so that YAML indentation is correct.
	proxyBlock := strings.Join(blockLines, "\n")

	// 2. If DefaultPodTemplate already ends exactly with our proxyBlock, do nothing.
	current := tc.Spec.Pipeline.DefaultPodTemplate
	if strings.HasSuffix(strings.TrimRight(current, "\n"), proxyBlock) {
		// Already has that exact "env:\n  - name ..." block, so skip.
		return
	}

	// 3. Otherwise, strip out any old proxy lines (any lines containing "name: HTTP_PROXY" etc.),
	//    and append the single proxyBlock with a leading newline.
	if strings.TrimSpace(current) == "" {
		// No existing content → just set it to proxyBlock (no extra newline at start).
		tc.Spec.Pipeline.DefaultPodTemplate = proxyBlock
		return
	}

	// Split into lines, filter out any old proxy-related lines and their "value:" siblings.
	lines := strings.Split(current, "\n")
	var filtered []string
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if strings.Contains(l, "name: HTTP_PROXY") ||
			strings.Contains(l, "name: HTTPS_PROXY") ||
			strings.Contains(l, "name: NO_PROXY") {
			// Skip this line plus the very next line (the "    value: ..." line).
			i++
			continue
		}
		filtered = append(filtered, l)
	}

	// Re‐join filtered lines, then append a newline + proxyBlock.
	tc.Spec.Pipeline.DefaultPodTemplate = strings.Join(filtered, "\n") + "\n" + proxyBlock
}*/


// ProxyTransformer builds a YAML fragment for default-pod-template that looks like:
//
//   env:
//     - name: HTTP_PROXY
//       value: "http://..."
//     - name: HTTPS_PROXY
//       value: "http://..."
//     - name: NO_PROXY
//       value: "10.96.0.1,*.cluster.local,*.svc"
//
// and injects it into tc.Spec.Pipeline.DefaultPodTemplate only once. By ensuring the resulting
// string always ends in a '\n', Kubernetes will print `default-pod-template: |` instead of `|-`.
func ProxyTransformer(tc *v1alpha1.TektonConfig) {
	http := os.Getenv("HTTP_PROXY")
	https := os.Getenv("HTTPS_PROXY")
	no := os.Getenv("NO_PROXY")

	// If none of the three proxy envs are set, do nothing.
	if http == "" && https == "" && no == "" {
		return
	}

	// 1. Build the canonical proxyBlock (no leading newline, but we'll add a trailing newline).
	lines := []string{
		"env:",
	}
	if http != "" {
		lines = append(lines,
			"  - name: HTTP_PROXY",
			fmt.Sprintf("    value: %q", http),
		)
	}
	if https != "" {
		lines = append(lines,
			"  - name: HTTPS_PROXY",
			fmt.Sprintf("    value: %q", https),
		)
	}
	if no != "" {
		lines = append(lines,
			"  - name: NO_PROXY",
			fmt.Sprintf("    value: %q", no),
		)
	}
	proxyBlock := strings.Join(lines, "\n")

	// 2. If DefaultPodTemplate already ends exactly with proxyBlock + "\n", do nothing.
	current := tc.Spec.Pipeline.DefaultPodTemplate
	desiredSuffix := proxyBlock + "\n"
	if strings.HasSuffix(current, desiredSuffix) {
		return
	}

	// 3. Otherwise, strip out any old proxy lines and create a new final string ending in '\n'.
	if strings.TrimSpace(current) == "" {
		// No existing content → just set to proxyBlock + "\n"
		tc.Spec.Pipeline.DefaultPodTemplate = desiredSuffix
		return
	}

	// Split into lines, filter out any old proxy-related lines (and their "value:" siblings).
	origLines := strings.Split(current, "\n")
	var filtered []string
	for i := 0; i < len(origLines); i++ {
		l := origLines[i]
		if strings.Contains(l, "name: HTTP_PROXY") ||
			strings.Contains(l, "name: HTTPS_PROXY") ||
			strings.Contains(l, "name: NO_PROXY") {
			// skip this line and its following "    value: ..." line
			i++
			continue
		}
		filtered = append(filtered, l)
	}

	// Re‐join filtered lines, append one newline, then proxyBlock + "\n"
	merged := strings.Join(filtered, "\n")
	if !strings.HasSuffix(merged, "\n") {
		merged = merged + "\n"
	}
	tc.Spec.Pipeline.DefaultPodTemplate = merged + proxyBlock + "\n"
}



