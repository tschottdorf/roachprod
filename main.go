package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	cld "github.com/cockroachdb/roachprod/cloud"
	"github.com/cockroachdb/roachprod/config"
	"github.com/cockroachdb/roachprod/install"
	"github.com/cockroachdb/roachprod/ssh"
	"github.com/cockroachdb/roachprod/ui"
	"github.com/cockroachdb/roachprod/vm"
	_ "github.com/cockroachdb/roachprod/vm/aws"
	"github.com/cockroachdb/roachprod/vm/gce"
	"github.com/cockroachdb/roachprod/vm/local"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

var rootCmd = &cobra.Command{
	Use:   "roachprod [command] (flags)",
	Short: "roachprod tool for manipulating test clusters",
	Long: `roachprod is a tool for manipulating ephemeral test clusters, allowing easy
creating, destruction, starting, stopping and wiping of clusters along with
running load generators.

Examples:

  roachprod create local -n 3
  roachprod start local
  roachprod sql local:2 -- -e "select * from crdb_internal.node_runtime_info"
  roachprod stop local
  roachprod wipe local
  roachprod destroy local

The above commands will create a "local" 3 node cluster, start a cockroach
cluster on these nodes, run a sql command on the 2nd node, stop, wipe and
destroy the cluster.
`,
}

var (
	numNodes       int
	numRacks       int
	username       string
	dryrun         bool
	extendLifetime time.Duration
	listDetails    bool
	listMine       bool
	clusterType    = "cockroach"
	secure         = false
	nodeEnv        = "COCKROACH_ENABLE_RPC_COMPRESSION=false"
	nodeArgs       []string
	tag            string
	external       = false
	adminurlOpen   = false
	useSCP         = true
)

func sortedClusters() []string {
	var r []string
	for n := range install.Clusters {
		r = append(r, n)
	}
	sort.Strings(r)
	return r
}

func newCluster(name string, reserveLoadGen bool) (*install.SyncedCluster, error) {
	nodeNames := "all"
	{
		parts := strings.Split(name, ":")
		switch len(parts) {
		case 2:
			nodeNames = parts[1]
			fallthrough
		case 1:
			name = parts[0]
		case 0:
			return nil, fmt.Errorf("no cluster specified")
		default:
			return nil, fmt.Errorf("invalid cluster name: %s", name)
		}
	}

	c, ok := install.Clusters[name]
	if !ok {
		return nil, fmt.Errorf(`unknown cluster: %s

Available clusters:
  %s

Hint: use "roachprod sync" to update the list of available clusters.
`,
			name, strings.Join(sortedClusters(), "\n  "))
	}

	switch clusterType {
	case "cockroach":
		c.Impl = install.Cockroach{}
		if numRacks > 0 {
			for i := range c.Localities {
				rack := fmt.Sprintf("rack=%d", i%numRacks)
				if c.Localities[i] != "" {
					rack = "," + rack
				}
				c.Localities[i] += rack
			}
		}
	case "cassandra":
		c.Impl = install.Cassandra{}
	default:
		return nil, fmt.Errorf("unknown cluster type: %s", clusterType)
	}

	nodes, err := install.ListNodes(nodeNames, len(c.VMs))
	if err != nil {
		return nil, err
	}

	c.Nodes = nodes
	if reserveLoadGen {
		// TODO(marc): make loadgen node configurable. For now, we always use the
		// last ID (1-indexed).
		c.LoadGen = len(c.VMs)
	} else {
		c.LoadGen = -1
	}
	c.Secure = secure
	c.Env = nodeEnv
	c.Args = nodeArgs
	if tag != "" {
		c.Tag = "/" + tag
	}
	c.UseSCP = useSCP
	return c, nil
}

// verifyClusterName ensures that the given name conforms to
// our naming pattern of "<username>-<clustername>". The
// username must match one of the vm.Provider account names
// or the --username override.
func verifyClusterName(clusterName string) (string, error) {
	if len(clusterName) == 0 {
		return "", fmt.Errorf("cluster name cannot be blank")
	}
	if clusterName == config.Local {
		return clusterName, nil
	}

	// Use the vm.Provider account names, or --username.
	var accounts []string
	if len(username) > 0 {
		accounts = []string{username}
	} else {
		seenAccounts := map[string]bool{}
		active, err := vm.FindActiveAccounts()
		if err != nil {
			return "", err
		}
		for _, account := range active {
			if !seenAccounts[account] {
				seenAccounts[account] = true
				accounts = append(accounts, account)
			}
		}
	}

	// If we see <account>-<something>, accept it.
	for _, account := range accounts {
		if strings.HasPrefix(clusterName, account+"-") && len(clusterName) > len(account)+1 {
			return clusterName, nil
		}
	}

	// Try to pick out a reasonable cluster name from the input.
	i := strings.Index(clusterName, "-")
	suffix := clusterName
	if i != -1 {
		// The user specified a username prefix, but it didn't match an active
		// account name. For example, assuming the account is "peter", `roachprod
		// create joe-perf` should be specified as `roachprod create joe-perf -u
		// joe`.
		suffix = clusterName[i+1:]
	} else {
		// The user didn't specify a username prefix. For example, assuming the
		// account is "peter", `roachprod create perf` should be specified as
		// `roachprod create peter-perf`.
	}

	// Suggest acceptable cluster names.
	var suggestions []string
	for _, account := range accounts {
		suggestions = append(suggestions, fmt.Sprintf("%s-%s", account, suffix))
	}
	return "", fmt.Errorf("malformed cluster name %s, did you mean one of %s",
		clusterName, suggestions)
}

func wrap(f func(cmd *cobra.Command, args []string) error) func(cmd *cobra.Command, args []string) {
	return func(cmd *cobra.Command, args []string) {
		err := f(cmd, args)
		if err != nil {
			cmd.Println("Error: ", err.Error())
			os.Exit(1)
		}
	}
}

var createVMOpts vm.CreateOpts

var createCmd = &cobra.Command{
	Use:   "create <cluster>",
	Short: "create a cluster",
	Long: `Create a local or cloud-based cluster.

A cluster is composed of a set of nodes, configured during cluster creation via
the --nodes flag. Creating a cluster does not start any processes on the nodes
other than the base system processes (e.g. sshd). See "roachprod start" for
starting cockroach nodes and "roachprod {run,ssh}" for running arbitrary
commands on the nodes of a cluster.

Cloud Clusters

  Cloud-based clusters are ephemeral and come with a lifetime (specified by the
  --lifetime flag) after which they will be automatically
  destroyed. Cloud-based clusters require the associated command line tool for
  the cloud to be installed and configured (e.g. "gcloud auth login").

  Clusters names are required to be prefixed by the authenticated user of the
  cloud service. The suffix is an arbitrary string used to distinguish
  clusters. For example, "marc-test" is a valid cluster name for the user
  "marc". The authenticated user for the cloud service is automatically
  detected and can be override by the ROACHPROD_USER environment variable or
  the --username flag.

  The machine type and the use of local SSD storage can be specified during
  cluster creation via the --{cloud}-machine-type and --local-ssd flags. The
  machine-type is cloud specified. For example, --gce-machine-type=n1-highcpu-8
  requests the "n1-highcpu-8" machine type for a GCE-based cluster. No attempt
  is made (or desired) to abstract machine types across cloud providers. See
  the cloud provider's documentation for details on the machine types
  available.

Local Clusters

  A local cluster stores the per-node data in ${HOME}/local on the machine
  roachprod is being run on. Local clusters requires local ssh access. Unlike
  cloud clusters there can be only a single local cluster, the local cluster is
  always named "local", and has no expiration (unlimited lifetime).
`,
	Args: cobra.ExactArgs(1),
	Run: wrap(func(cmd *cobra.Command, args []string) error {
		if numNodes <= 0 || numNodes >= 1000 {
			// Upper limit is just for safety.
			return fmt.Errorf("number of nodes must be in [1..999]")
		}

		clusterName, err := verifyClusterName(args[0])
		if err != nil {
			return err
		}

		if clusterName != config.Local {
			cloud, err := cld.ListCloud()
			if err != nil {
				return err
			}
			if _, ok := cloud.Clusters[clusterName]; ok {
				return fmt.Errorf("cluster %s already exists", clusterName)
			}
		} else {
			if _, ok := install.Clusters[clusterName]; ok {
				return fmt.Errorf("cluster %s already exists", clusterName)
			}

			// If the local cluster is being created, force the local Provider to be used
			createVMOpts.VMProviders = []string{local.ProviderName}
		}

		fmt.Printf("Creating cluster %s with %d nodes\n", clusterName, numNodes)
		if createErr := cld.CreateCluster(clusterName, numNodes, createVMOpts); createErr == nil {
			fmt.Println("OK")
		} else if clusterName == config.Local {
			return createErr
		} else {
			fmt.Fprintf(os.Stderr, "Unable to create cluster:\n%s\nCleaning up...\n", createErr)
			if err := cleanupFailedCreate(clusterName); err != nil {
				fmt.Fprintf(os.Stderr, "Error while cleaning up partially-created cluster: %s\n", err)
			}
			os.Exit(1)
		}

		if clusterName != config.Local {
			{
				cloud, err := cld.ListCloud()
				if err != nil {
					return err
				}

				c, ok := cloud.Clusters[clusterName]
				if !ok {
					return fmt.Errorf("could not find %s in list of cluster", clusterName)
				}
				c.PrintDetails()

				// Run ssh-keygen -R serially on each new VM in case an IP address has been recycled
				for _, v := range c.VMs {
					cmd := exec.Command("ssh-keygen", "-R", v.PublicIP)
					out, err := cmd.CombinedOutput()
					if err != nil {
						log.Printf("could not clear ssh key for hostname %s:\n%s", v.PublicIP, string(out))
					}
				}

				if err := syncAll(cloud); err != nil {
					return err
				}
			}

			{
				// Wait for the nodes in the cluster to start.
				install.Clusters = map[string]*install.SyncedCluster{}
				if err := loadClusters(); err != nil {
					return err
				}

				c, err := newCluster(clusterName, false)
				if err != nil {
					return err
				}

				if err := c.Wait(); err != nil {
					return err
				}
			}
		} else {
			for i := 0; i < numNodes; i++ {
				err := os.MkdirAll(fmt.Sprintf(os.ExpandEnv("${HOME}/local/%d"), i+1), 0755)
				if err != nil {
					return err
				}
			}
		}

		return nil
	}),
}

func cleanupFailedCreate(clusterName string) error {
	cloud, err := cld.ListCloud()
	if err != nil {
		return err
	}
	c, ok := cloud.Clusters[clusterName]
	if !ok {
		// If the cluster doesn't exist, we didn't manage to create any VMs
		// before failing. Not an error.
		return nil
	}
	return cld.DestroyCluster(c)
}

var destroyCmd = &cobra.Command{
	Use:   "destroy <cluster>",
	Short: "destroy a cluster",
	Long: `Destroy a local or cloud-based cluster.

Destroying a cluster releases the resources for a cluster. For a cloud-based
cluster the machine and associated disk resources are freed. For a local
cluster, any processes started by roachprod are stopped, and the ${HOME}/local
directory is removed.
`,
	Args: cobra.ExactArgs(1),
	Run: wrap(func(cmd *cobra.Command, args []string) error {
		clusterName, err := verifyClusterName(args[0])
		if err != nil {
			return err
		}

		if clusterName != config.Local {
			cloud, err := cld.ListCloud()
			if err != nil {
				return err
			}

			c, ok := cloud.Clusters[clusterName]
			if !ok {
				return fmt.Errorf("cluster %s does not exist", clusterName)
			}

			fmt.Printf("Destroying cluster %s with %d nodes\n", clusterName, len(c.VMs))
			if err := cld.DestroyCluster(c); err != nil {
				return err
			}
		} else {
			if _, ok := install.Clusters[clusterName]; !ok {
				return fmt.Errorf("cluster %s does not exist", clusterName)
			}
			c, err := newCluster(clusterName, false /* reserveLoadGen */)
			if err != nil {
				return err
			}
			c.Wipe()
			for _, i := range c.Nodes {
				err := os.RemoveAll(fmt.Sprintf(os.ExpandEnv("${HOME}/local/%d"), i))
				if err != nil {
					return err
				}
			}
			if err := os.Remove(filepath.Join(os.ExpandEnv(config.DefaultHostDir), c.Name)); err != nil {
				return err
			}
		}

		fmt.Println("OK")
		return nil
	}),
}

var listCmd = &cobra.Command{
	Use:   "list [--details] [ --mine | <cluster name regex> ]",
	Short: "list all clusters",
	Long: `List all clusters.

The list command accepts an optional positional argument, which is a regular
expression that will be matched against the cluster name pattern.  Alternatively,
the --mine flag can be provided to list the clusters that are owned by the current
user.

The default output shows one line per cluster, including the local cluster if
it exists:

  ~ roachprod list
  local:     [local]    1  (-)
  marc-test: [aws gce]  4  (5h34m35s)
  Syncing...

The second column lists the cloud providers that host VMs for the cluster.

The third and fourth columns are the number of nodes in the cluster and the
time remaining before the cluster will be automatically destroyed. Note that
local clusters do not have an expiration.

The --details adjusts the output format to include per-node details:

  ~ roachprod list --details
  local [local]: (no expiration)
    localhost		127.0.0.1	127.0.0.1
  marc-test: [aws gce] 5h33m57s remaining
    marc-test-0001	marc-test-0001.us-east1-b.cockroach-ephemeral	10.142.0.18	35.229.60.91
    marc-test-0002	marc-test-0002.us-east1-b.cockroach-ephemeral	10.142.0.17	35.231.0.44
    marc-test-0003	marc-test-0003.us-east1-b.cockroach-ephemeral	10.142.0.19	35.229.111.100
    marc-test-0004	marc-test-0004.us-east1-b.cockroach-ephemeral	10.142.0.20	35.231.102.125
  Syncing...

The first and second column are the node hostname and fully qualified name
respectively. The third and fourth column are the private and public IP
addresses.

Listing clusters has the side-effect of syncing ssh keys/configs and the local
hosts file.
`,
	Run: wrap(func(cmd *cobra.Command, args []string) error {
		listPattern := regexp.MustCompile(".*")
		switch len(args) {
		case 0:
			if listMine {
				// In general, we expect that users will have the same
				// account name across the services they're using,
				// but we still want to function even if this is not
				// the case.
				seenAccounts := map[string]bool{}
				accounts, err := vm.FindActiveAccounts()
				if err != nil {
					return err
				}
				pattern := ""
				for _, account := range accounts {
					if !seenAccounts[account] {
						seenAccounts[account] = true
						if len(pattern) > 0 {
							pattern += "|"
						}
						pattern += fmt.Sprintf("(^%s-)", regexp.QuoteMeta(account))
					}
				}
				listPattern, err = regexp.Compile(pattern)
				if err != nil {
					return err
				}
			}
		case 1:
			if listMine {
				return errors.New("--mine cannot be combined with a pattern")
			}
			var err error
			listPattern, err = regexp.Compile(args[0])
			if err != nil {
				return errors.Wrapf(err, "could not compile regex pattern: %s", args[0])
			}
		default:
			return errors.New("only a single pattern may be listed")
		}

		cloud, err := cld.ListCloud()
		if err != nil {
			return err
		}

		// Filter and sort by cluster names for stable output
		var names []string
		for name := range cloud.Clusters {
			if listPattern.MatchString(name) {
				names = append(names, name)
			}
		}
		sort.Strings(names)

		// Align columns left and separate with at least two spaces.
		tw := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
		for _, name := range names {
			c := cloud.Clusters[name]
			if listDetails {
				c.PrintDetails()
			} else {
				fmt.Fprintf(tw, "%s:\t%s\t%d", c.Name, c.Clouds(), len(c.VMs))
				if !c.IsLocal() {
					fmt.Fprintf(tw, "\t(%s)", c.LifetimeRemaining().Round(time.Second))
				} else {
					fmt.Fprintf(tw, "\t(-)")
				}
				fmt.Fprintf(tw, "\n")
			}
		}
		if err := tw.Flush(); err != nil {
			return err
		}

		// Optionally print any dangling instances with errors
		if listDetails {
			collated := cloud.BadInstanceErrors()

			// Sort by Error() value for stable output
			var errors ui.ErrorsByError
			for err := range collated {
				errors = append(errors, err)
			}
			sort.Sort(errors)

			for _, e := range errors {
				fmt.Printf("%s: %s\n", e, collated[e].Names())
			}
		}

		return syncAll(cloud)
	}),
}

// TODO(peter): Do we need this command given that the "list" command syncs as
// a side-effect. If you don't care about the list output, just "roachprod list
// &>/dev/null".
var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "sync ssh keys/config and hosts files",
	Long:  ``,
	Run: wrap(func(cmd *cobra.Command, args []string) error {
		cloud, err := cld.ListCloud()
		if err != nil {
			return err
		}
		return syncAll(cloud)
	}),
}

var lockFile = os.ExpandEnv("$HOME/.roachprod/LOCK")

var bashCompletion = os.ExpandEnv("$HOME/.roachprod/bash-completion.sh")

func syncAll(cloud *cld.Cloud) error {
	fmt.Println("Syncing...")

	// Acquire a filesystem lock so that two concurrent `roachprod sync`
	// operations don't clobber each other.
	f, err := os.Create(lockFile)
	if err != nil {
		return errors.Wrapf(err, "creating lock file %q", lockFile)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return errors.Wrap(err, "acquiring lock on %q")
	}
	defer f.Close()

	if err := syncHosts(cloud); err != nil {
		return err
	}
	err = vm.ProvidersSequential(vm.AllProviderNames(), func(p vm.Provider) error {
		return p.CleanSSH()
	})
	if err != nil {
		return err
	}

	{
		names := make([]string, 0, len(cloud.Clusters)*3)
		for name, c := range cloud.Clusters {
			names = append(names, name)
			for i := range c.VMs {
				names = append(names, fmt.Sprintf("%s:%d", name, i))
			}
		}
		for _, cmd := range []*cobra.Command{
			startCmd, stopCmd, wipeCmd,
			extendCmd, destroyCmd,
			statusCmd, monitorCmd,
			runCmd, sqlCmd,
			adminurlCmd, pgurlCmd,
		} {
			cmd.ValidArgs = names
		}
		rootCmd.GenBashCompletionFile(bashCompletion)
	}
	return vm.ProvidersSequential(vm.AllProviderNames(), func(p vm.Provider) error {
		return p.ConfigSSH()
	})
}

var gcCmd = &cobra.Command{
	Use:   "gc",
	Short: "GC expired clusters\n",
	Long: `Garbage collect expired clusters.

Destroys expired clusters, sending email if properly configured. Usually run
hourly by a cronjob so it is not necessary to run manually.
`,
	Run: wrap(func(cmd *cobra.Command, args []string) error {
		cloud, err := cld.ListCloud()
		if err != nil {
			return err
		}
		return cld.GCClusters(cloud, dryrun)
	}),
}

var extendCmd = &cobra.Command{
	Use:   "extend <cluster>",
	Short: "extend the lifetime of a cluster",
	Long: `Extend the lifetime of the specified cluster to prevent it from being
destroyed:

  roachprod extend marc-test --lifetime=6h
`,
	Args: cobra.ExactArgs(1),
	Run: wrap(func(cmd *cobra.Command, args []string) error {
		clusterName, err := verifyClusterName(args[0])
		if err != nil {
			return err
		}

		cloud, err := cld.ListCloud()
		if err != nil {
			return err
		}

		c, ok := cloud.Clusters[clusterName]
		if !ok {
			return fmt.Errorf("cluster %s does not exist", clusterName)
		}

		if err := cld.ExtendCluster(c, extendLifetime); err != nil {
			return err
		}

		// Reload the clusters and print details.
		cloud, err = cld.ListCloud()
		if err != nil {
			return err
		}

		c, ok = cloud.Clusters[clusterName]
		if !ok {
			return fmt.Errorf("cluster %s does not exist", clusterName)
		}

		c.PrintDetails()
		return nil
	}),
}

const tagHelp = `
The --tag flag can be used to to associate a tag with the process. This tag can
then be used to restrict the processes which are operated on by the status and
stop commands. Tags can have a hierarchical component by utilizing a slash
separated string similar to a filesystem path. A tag matches if a prefix of the
components match. For example, the tag "a/b" will match both "a/b" and
"a/b/c/d".
`

var startCmd = &cobra.Command{
	Use:   "start <cluster>",
	Short: "start nodes on a cluster",
	Long: `Start nodes on a cluster.

The --secure flag can be used to start nodes in secure mode (i.e. using
certs). When specified, there is a one time initialization for the cluster to
create and distribute the certs. Note that running some modes in secure mode
and others in insecure mode is not a supported Cockroach configuration.

As a debugging aid, the --sequential flag starts the nodes sequentially so node
IDs match hostnames. Otherwise nodes are started are parallel.

The --binary flag specifies the remote binary to run. It is up to the roachprod
user to ensure this binary exists, usually via "roachprod put". Note that no
cockroach software is installed by default on a newly created cluster.

The --args and --env flags can be used to pass arbitrary command line flags and
environment variables to the cockroach process.
` + tagHelp + `
The "start" command takes care of setting up the --join address and specifying
reasonable defautls for other flags. One side-effect of this convenience is
that node 1 is special and must be started for the cluster to be initialized.

If the COCKROACH_DEV_LICENSE environment variable is set the enterprise.license
cluster setting will be set to its value.
`,
	Args: cobra.ExactArgs(1),
	Run: wrap(func(cmd *cobra.Command, args []string) error {
		c, err := newCluster(args[0], false /* reserveLoadGen */)
		if err != nil {
			return err
		}
		c.Start()
		return nil
	}),
}

var stopCmd = &cobra.Command{
	Use:   "stop <cluster>",
	Short: "stop nodes on a cluster",
	Long: `Stop nodes on a cluster.

Stop roachprod created processes running on the nodes in a cluster, including
processes started by the "start", "run" and "ssh" commands. Every process
started by roachprod is tagged with a ROACHPROD=<node> environment variable
which is used by "stop" to locate the processes and terminate them. Processes
are killed with signal 9 (SIGKILL) giving them no chance for a graceful exit.
` + tagHelp + `
`,
	Args: cobra.ExactArgs(1),
	Run: wrap(func(cmd *cobra.Command, args []string) error {
		c, err := newCluster(args[0], false /* reserveLoadGen */)
		if err != nil {
			return err
		}
		c.Stop()
		return nil
	}),
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "retrieve the status of nodes in a cluster",
	Long: `Retrieve the status of nodes in a cluster.

The "status" command outputs the binary and PID for the specified nodes:

  ~ roachprod status local
  local: status 3/3
     1: cockroach 29688
     2: cockroach 29687
     3: cockroach 29689
` + tagHelp + `
`,
	Args: cobra.ExactArgs(1),
	Run: wrap(func(cmd *cobra.Command, args []string) error {
		c, err := newCluster(args[0], false /* reserveLoadGen */)
		if err != nil {
			return err
		}
		c.Status()
		return nil
	}),
}

var monitorCmd = &cobra.Command{
	Use:   "monitor",
	Short: "monitor the status of nodes in a cluster",
	Long: `Monitor the status of cockroach nodes in a cluster.

The "monitor" command runs until terminated. At startup it outputs a line for
each specified node indicating the status of the node (either the PID of the
node if alive, or "dead" otherwise). It then watches for changes in the status
of nodes, outputting a line whenever a change is detected:

  ~ roachprod monitor local
  1: 29688
  3: 29689
  2: 29687
  3: dead
  3: 30718
`,
	Args: cobra.ExactArgs(1),
	Run: wrap(func(cmd *cobra.Command, args []string) error {
		c, err := newCluster(args[0], false /* reserveLoadGen */)
		if err != nil {
			return err
		}
		for i := range c.Monitor() {
			fmt.Printf("%d: %s\n", i.Index, i.Msg)
		}
		return nil
	}),
}

var wipeCmd = &cobra.Command{
	Use:   "wipe <cluster>",
	Short: "wipe a cluster",
	Long: `Wipe the nodes in a cluster.

The "wipe" command first stops any processes running on the nodes in a cluster
(via the "stop" command) and then deletes the data directories used by the
nodes.
`,
	Args: cobra.ExactArgs(1),
	Run: wrap(func(cmd *cobra.Command, args []string) error {
		c, err := newCluster(args[0], false /* reserveLoadGen */)
		if err != nil {
			return err
		}
		c.Wipe()
		return nil
	}),
}

var runCmd = &cobra.Command{
	Use:     "run <cluster> <command> [args]",
	Aliases: []string{"ssh"},
	Short:   "run a command on the nodes in a cluster",
	Long: `Run a command on the nodes in a cluster.
`,
	Args: cobra.MinimumNArgs(1),
	Run: wrap(func(_ *cobra.Command, args []string) error {
		c, err := newCluster(args[0], false /* reserveLoadGen */)
		if err != nil {
			return err
		}

		// Use "ssh" if an interactive session was requested (i.e. there is no
		// remote command to run).
		if len(args) == 1 {
			return c.Ssh(nil, args[1:])
		}

		cmd := strings.TrimSpace(strings.Join(args[1:], " "))
		title := cmd
		if len(title) > 30 {
			title = title[:27] + "..."
		}
		return c.Run(os.Stdout, os.Stderr, c.Nodes, title, cmd)
	}),
}

var testCmd = &cobra.Command{
	Use:   "test <cluster> <name>...",
	Short: "run one or more tests on a cluster",
	Long: `Run one or more tests on a cluster. The test <name> must be one of:

	` + strings.Join(allTests(), "\n\t") + `

Alternately, an interrupted test can be resumed by specifying the output
directory of a previous test. For example:

	roachperf test denim kv_0.cockroach-6151ae1

will restart the kv_0 test on denim using the cockroach binary with the build
tag 6151ae1.`,
	Args: cobra.MinimumNArgs(2),
	Run: wrap(func(cmd *cobra.Command, args []string) error {
		for _, arg := range args[1:] {
			if err := runTest(arg, args[0]); err != nil {
				return err
			}
		}
		return nil
	}),
}

var installCmd = &cobra.Command{
	Use:   "install <cluster> <software>",
	Short: "install 3rd party software",
	Long: `Install third party software. Currently available installation options are:

  cassandra
  mongodb
  postgres
  tools (fio, iftop, perf)
`,
	Args: cobra.MinimumNArgs(2),
	Run: wrap(func(cmd *cobra.Command, args []string) error {
		c, err := newCluster(args[0], false /* reserveLoadGen */)
		if err != nil {
			return err
		}
		return install.Install(c, args[1:])
	}),
}

var putCmd = &cobra.Command{
	Use:   "put <cluster> <src> [<dest>]",
	Short: "copy a local file to the nodes in a cluster",
	Long: `Copy a local file to the nodes in a cluster.
`,
	Args: cobra.RangeArgs(2, 3),
	Run: wrap(func(cmd *cobra.Command, args []string) error {
		src := args[1]
		dest := path.Base(src)
		if len(args) == 3 {
			dest = args[2]
		}
		c, err := newCluster(args[0], false /* reserveLoadGen */)
		if err != nil {
			return err
		}
		c.Put(src, dest)
		return nil
	}),
}

var getCmd = &cobra.Command{
	Use:   "get <cluster> <src> [<dest>]",
	Short: "copy a remote file from the nodes in a cluster",
	Long: `Copy a remote file from the nodes in a cluster. If the file is retrieved from
multiple nodes the destination file name will be prefixed with the node number.
`,
	Args: cobra.RangeArgs(2, 3),
	Run: wrap(func(cmd *cobra.Command, args []string) error {
		src := args[1]
		dest := path.Base(src)
		if len(args) == 3 {
			dest = args[2]
		}
		c, err := newCluster(args[0], false /* reserveLoadGen */)
		if err != nil {
			return err
		}
		c.Get(src, dest)
		return nil
	}),
}

var sqlCmd = &cobra.Command{
	Use:   "sql <cluster> -- [args]",
	Short: "run `cockroach sql` on a remote cluster",
	Long:  "Run `cockroach sql` on a remote cluster.\n",
	Args:  cobra.MinimumNArgs(1),
	Run: wrap(func(cmd *cobra.Command, args []string) error {
		c, err := newCluster(args[0], false /* reserveLoadGen */)
		if err != nil {
			return err
		}
		cockroach, ok := c.Impl.(install.Cockroach)
		if !ok {
			return errors.New("sql is only valid on cockroach clusters")
		}
		return cockroach.SQL(c, args[1:])
	}),
}

var pgurlCmd = &cobra.Command{
	Use:   "pgurl <cluster>",
	Short: "generate pgurls for the nodes in a cluster",
	Long: `Generate pgurls for the nodes in a cluster.
`,
	Args: cobra.ExactArgs(1),
	Run: wrap(func(cmd *cobra.Command, args []string) error {
		c, err := newCluster(args[0], false /* reserveLoadGen */)
		if err != nil {
			return err
		}
		nodes := c.ServerNodes()
		ips := make([]string, len(nodes))

		if external {
			for i := 0; i < len(nodes); i++ {
				ips[i] = c.VMs[nodes[i]-1]
			}
		} else {
			c.Parallel("", len(nodes), 0, func(i int) ([]byte, error) {
				var err error
				ips[i], err = c.GetInternalIP(nodes[i])
				return nil, err
			})
		}

		var urls []string
		for i, ip := range ips {
			urls = append(urls, c.Impl.NodeURL(c, ip, c.Impl.NodePort(c, nodes[i])))
		}
		fmt.Println(strings.Join(urls, " "))
		return nil
	}),
}

var adminurlCmd = &cobra.Command{
	Use:     "adminurl <cluster>",
	Aliases: []string{"admin", "adminui"},
	Short:   "generate admin UI URLs for the nodes in a cluster\n",
	Long: `Generate admin UI URLs for the nodes in a cluster.
`,
	Args: cobra.ExactArgs(1),
	Run: wrap(func(cmd *cobra.Command, args []string) error {
		c, err := newCluster(args[0], false /* reserveLoadGen */)
		if err != nil {
			return err
		}

		for _, node := range c.ServerNodes() {
			ip := c.VMs[node-1]
			port := install.GetAdminUIPort(c.Impl.NodePort(c, node))
			scheme := "http"
			if c.Secure {
				scheme = "https"
			}
			url := fmt.Sprintf("%s://%s:%d/", scheme, ip, port)
			if adminurlOpen {
				if err := exec.Command("python", "-m", "webbrowser", url).Run(); err != nil {
					return err
				}
			} else {
				fmt.Println(url)
			}
		}
		return nil
	}),
}

func init() {
	adminurlCmd.Flags().BoolVar(&adminurlOpen, `open`, false, `Open the url in a browser`)
}

var webCmd = &cobra.Command{
	Use:   "web <testdir> [<testdir>]",
	Short: "visualize and compare test output",
	Long: `Visualize test output.

The "web" command can visualize the output of a single test or compare the
output of two or more tests.
`,
	Args: cobra.MinimumNArgs(1),
	Run: wrap(func(cmd *cobra.Command, args []string) error {
		return web(args)
	}),
}

var dumpCmd = &cobra.Command{
	Use:   "dump <testdir> [<testdir>]",
	Short: "dump test output",
	Long: `Display test output.

The "dump" command can display the output of a single test or compare the
output of two tests.
`,
	Args: cobra.RangeArgs(1, 2),
	Run: wrap(func(cmd *cobra.Command, args []string) error {
		return dump(args)
	}),
}

func main() {
	// The commands are displayed in the order they are added to rootCmd. Note
	// that gcCmd and adminurlCmd contain a trailing \n in their Short help in
	// order to separate the commands into logical groups.
	cobra.EnableCommandSorting = false
	rootCmd.AddCommand(
		createCmd,
		destroyCmd,
		extendCmd,
		listCmd,
		syncCmd,
		gcCmd,

		statusCmd,
		monitorCmd,
		startCmd,
		stopCmd,
		runCmd,
		wipeCmd,
		testCmd,
		installCmd,
		putCmd,
		getCmd,
		sqlCmd,
		pgurlCmd,
		adminurlCmd,

		webCmd,
		dumpCmd,
	)

	for _, cmd := range []*cobra.Command{createCmd, destroyCmd, extendCmd} {
		cmd.Flags().StringVarP(&username, "username", "u", os.Getenv("ROACHPROD_USER"),
			"Username to run under, detect if blank")
	}

	for _, cmd := range []*cobra.Command{statusCmd, monitorCmd, startCmd,
		stopCmd, runCmd, wipeCmd, testCmd, installCmd, putCmd, getCmd,
		sqlCmd, pgurlCmd, adminurlCmd,
	} {
		cmd.Flags().BoolVar(
			&ssh.InsecureIgnoreHostKey, "insecure-ignore-host-key", true, "don't check ssh host keys")
	}

	createCmd.Flags().DurationVarP(&createVMOpts.Lifetime,
		"lifetime", "l", 12*time.Hour, "Lifetime of the cluster")
	createCmd.Flags().BoolVar(&createVMOpts.UseLocalSSD,
		"local-ssd", true, "Use local SSD")
	createCmd.Flags().IntVarP(&numNodes,
		"nodes", "n", 4, "Total number of nodes, distributed across all clouds")
	createCmd.Flags().StringSliceVarP(&createVMOpts.VMProviders,
		"clouds", "c", []string{gce.ProviderName},
		fmt.Sprintf("The cloud provider(s) to use when creating new vm instances: %s", vm.AllProviderNames()))
	createCmd.Flags().BoolVar(&createVMOpts.GeoDistributed,
		"geo", false, "Create geo-distributed cluster")
	// Allow each Provider to inject additional configuration flags
	for _, p := range vm.Providers {
		p.Flags().ConfigureCreateFlags(createCmd.Flags())

		for _, cmd := range []*cobra.Command{
			createCmd, destroyCmd, extendCmd, listCmd, syncCmd, gcCmd,
		} {
			p.Flags().ConfigureClusterFlags(cmd.Flags())
		}
	}

	extendCmd.Flags().DurationVarP(&extendLifetime,
		"lifetime", "l", 12*time.Hour, "Lifetime of the cluster")

	listCmd.Flags().BoolVarP(&listDetails,
		"details", "d", false, "Show cluster details")
	listCmd.Flags().BoolVarP(&listMine,
		"mine", "m", false, "Show only clusters belonging to the current user")

	gcCmd.Flags().BoolVarP(
		&dryrun, "dry-run", "n", dryrun, "dry run (don't perform any actions)")
	gcCmd.Flags().StringVar(&config.SlackToken, "slack-token", "", "Slack bot token")

	pgurlCmd.Flags().BoolVar(
		&external, "external", false, "return pgurls for external connections")

	runCmd.Flags().BoolVar(
		&secure, "secure", false, "use a secure cluster")

	startCmd.Flags().IntVarP(&numRacks,
		"racks", "r", 0, "the number of racks to partition the nodes into")

	testCmd.Flags().DurationVarP(
		&duration, "duration", "d", 5*time.Minute, "the duration to run each test")
	testCmd.Flags().StringVarP(
		&concurrency, "concurrency", "c", "1-64", "the concurrency to run each test")

	for _, cmd := range []*cobra.Command{
		startCmd, statusCmd, stopCmd, runCmd,
	} {
		cmd.Flags().StringVar(
			&tag, "tag", "", "the process tag")
	}

	for _, cmd := range []*cobra.Command{
		startCmd, putCmd, getCmd,
	} {
		cmd.Flags().BoolVar(
			&useSCP, "scp", useSCP, "use scp for file transfers")
	}

	for _, cmd := range []*cobra.Command{
		getCmd, putCmd, runCmd, startCmd, statusCmd, stopCmd, testCmd,
		wipeCmd, pgurlCmd, adminurlCmd, sqlCmd, installCmd,
	} {
		switch cmd {
		case startCmd, testCmd:
			cmd.Flags().StringVarP(
				&config.Binary, "binary", "b", config.Binary,
				"the remote cockroach binary used to start a server")
			cmd.Flags().BoolVar(
				&install.StartOpts.Sequential, "sequential", false,
				"start nodes sequentially so node IDs match hostnames")
			cmd.Flags().StringArrayVarP(
				&nodeArgs, "args", "a", nil, "node arguments")
			cmd.Flags().StringVarP(
				&nodeEnv, "env", "e", nodeEnv, "node environment variables")
			cmd.Flags().StringVarP(
				&clusterType, "type", "t", clusterType, `cluster type ("cockroach" or "cassandra")`)
			fallthrough
		case pgurlCmd, adminurlCmd, sqlCmd:
			cmd.Flags().BoolVar(
				&secure, "secure", false, "use a secure cluster")
		}

		if cmd.Long == "" {
			cmd.Long = cmd.Short
		}
		cmd.Long += fmt.Sprintf(`
Node specification

  By default the operation is performed on all nodes in <cluster>. A subset of
  nodes can be specified by appending :<nodes> to the cluster name. The syntax
  of <nodes> is a comma separated list of specific node IDs or range of
  IDs. For example:

    roachperf %[1]s marc-test:1-3,8-9

  will perform %[1]s on:

    marc-test-1
    marc-test-2
    marc-test-3
    marc-test-8
    marc-test-9
`, cmd.Name())
	}

	var err error
	config.OSUser, err = user.Current()
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to lookup current user: %s\n", err)
		os.Exit(1)
	}

	if err := initHostDir(); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}

	if err := loadClusters(); err != nil {
		// We don't want to exit as we may be looking at the help message.
		fmt.Printf("problem loading clusters: %s\n", err)
	}

	if err := rootCmd.Execute(); err != nil {
		// Cobra has already printed the error message.
		os.Exit(1)
	}
}
