package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ghodss/yaml"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/bookingcom/shipper/cmd/shipperctl/configurator"
	shipper "github.com/bookingcom/shipper/pkg/apis/shipper/v1alpha1"
	apputil "github.com/bookingcom/shipper/pkg/util/application"
	"github.com/bookingcom/shipper/pkg/util/filters"
	releaseutil "github.com/bookingcom/shipper/pkg/util/release"
)

const (
	decommissionedClustersFlagName = "decommissionedClusters"
)

var (
	decommissionedClusters []string
	dryrun                 bool
	printOption            string

	CleanCmd = &cobra.Command{
		Use:   "clean",
		Short: "clean Shipper objects",
	}

	cleanDeadClustersCmd = &cobra.Command{
		Use:   "decommissioned-clusters",
		Short: "clean Shipper releases from decommissioned clusters",
		Long: "deleting releases that are scheduled *only* on decommissioned clusters and are not contenders, " +
			"removing decommissioned clusters from annotations of releases that are scheduled partially on decommissioned clusters.",
		RunE: runCleanCommand,
	}

	CountCmd = &cobra.Command{
		Use:   "count",
		Short: "count Shipper releases that are scheduled *only* on decommissioned clusters",
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			switch printOption {
			case "", "json", "yaml":
				return
			default:
				cmd.Printf("error: output format %q not supported, allowed formats are: json, yaml\n", printOption)
				os.Exit(1)
			}
		},
	}

	countContendersCmd = &cobra.Command{
		Use:   "contender",
		Short: "count Shipper *contenders* that are scheduled *only* on decommissioned clusters",
		RunE:  runCountContenderCommand,
	}

	countReleasesCmd = &cobra.Command{
		Use:   "release",
		Short: "count Shipper *releases* that are scheduled *only* on decommissioned clusters",
		RunE:  runCountReleasesCommand,
	}
)

type OutputRelease struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func init() {
	// Flags common to all commands under `shipperctl clean/count`
	for _, command := range []*cobra.Command{CountCmd, CleanCmd} {
		command.PersistentFlags().StringVar(&kubeConfigFile, kubeConfigFlagName, "~/.kube/config", "the path to the Kubernetes configuration file")
		if err := command.MarkPersistentFlagFilename(kubeConfigFlagName, "yaml"); err != nil {
			command.Printf("warning: could not mark %q for filename autocompletion: %s\n", kubeConfigFlagName, err)
		}

		command.PersistentFlags().BoolVar(&dryrun, "dryrun", false, "If true, only prints the objects that will be modifies/deleted")
		command.PersistentFlags().StringVar(&managementClusterContext, "management-cluster-context", "", "The name of the context to use to communicate with the management cluster. defaults to the current one")
		command.PersistentFlags().StringSliceVar(&decommissionedClusters, decommissionedClustersFlagName, decommissionedClusters, "List of decommissioned clusters. (Required)")
		if err := command.MarkPersistentFlagRequired(decommissionedClustersFlagName); err != nil {
			command.Printf("warning: could not mark %q as required: %s\n", decommissionedClustersFlagName, err)
		}

	}
	// Flags common to all commands under `shipperctl count`
	CountCmd.PersistentFlags().StringVarP(&printOption, "output", "o", "", "Output format. One of: json|yaml. Optional")

	CleanCmd.AddCommand(cleanDeadClustersCmd)
	CountCmd.AddCommand(countContendersCmd)
	CountCmd.AddCommand(countReleasesCmd)
}

func runCleanCommand(cmd *cobra.Command, args []string) error {
	configurator, err := configurator.NewClusterConfiguratorFromKubeConfig(kubeConfigFile, managementClusterContext)
	if err != nil {
		return err
	}

	namespaceList, err := configurator.KubeClient.CoreV1().Namespaces().List(metav1.ListOptions{})
	if err != nil {
		return err
	}
	var errList []string
	for _, ns := range namespaceList.Items {
		releaseList, err := configurator.ShipperClient.ShipperV1alpha1().Releases(ns.Name).List(metav1.ListOptions{})
		if err != nil {
			errList = append(errList, err.Error())
			continue
		}
		for _, rel := range releaseList.Items {
			trueClusters := getFilteredSelectedClusters(&rel)
			if len(trueClusters) > 0 {
				sort.Strings(trueClusters)

				if strings.Join(trueClusters, ",") == rel.Annotations[shipper.ReleaseClustersAnnotation] {
					continue
				}
				rel.Annotations[shipper.ReleaseClustersAnnotation] = strings.Join(trueClusters, ",")
				cmd.Printf("Editing annotations of release %s/%s to %s...", rel.Namespace, rel.Name, rel.Annotations[shipper.ReleaseClustersAnnotation])
				if !dryrun {
					_, err := configurator.ShipperClient.ShipperV1alpha1().Releases(ns.Name).Update(&rel)
					if err != nil {
						errList = append(errList, err.Error())
					}
					cmd.Println("done")
				} else {
					cmd.Println("dryrun")
				}
				continue
			}
			isContender, err := isContender(&rel, configurator)
			if err != nil {
				errList = append(errList, err.Error())
				continue
			}
			if len(trueClusters) == 0 && !isContender {
				cmd.Printf("Deleting release %s/%s...", rel.Namespace, rel.Name)
				if !dryrun {
					err := configurator.ShipperClient.ShipperV1alpha1().Releases(ns.Name).Delete(rel.Name, &metav1.DeleteOptions{})
					if err != nil {
						errList = append(errList, err.Error())
					}
					cmd.Println("done")
				} else {
					cmd.Println("dryrun")
				}
			}
		}
	}

	if len(errList) > 0 {
		return fmt.Errorf(strings.Join(errList, ","))
	}
	return nil
}

func runCountContenderCommand(cmd *cobra.Command, args []string) error {
	counter := 0
	configurator, err := configurator.NewClusterConfiguratorFromKubeConfig(kubeConfigFile, managementClusterContext)
	if err != nil {
		return err
	}

	namespaceList, err := configurator.KubeClient.CoreV1().Namespaces().List(metav1.ListOptions{})
	if err != nil {
		return err
	}
	var errList []string
	var countedReleases []OutputRelease
	for _, ns := range namespaceList.Items {
		applicationList, err := configurator.ShipperClient.ShipperV1alpha1().Applications(ns.Name).List(metav1.ListOptions{})
		if err != nil {
			errList = append(errList, err.Error())
			continue
		}
		for _, app := range applicationList.Items {
			contender, err := getContender(&app, configurator)
			if err != nil {
				errList = append(errList, err.Error())
				continue
			}
			trueClusters := getFilteredSelectedClusters(contender)
			if len(trueClusters) == 0 {
				counter++
				countedReleases = append(
					countedReleases,
					OutputRelease{
						Namespace: contender.Namespace,
						Name:      contender.Name,
					})
			}
		}
	}

	if printOption == "" {
		cmd.Println("Number of *contenders* that are scheduled only on decommissioned clusters: ", counter)
	} else {
		printCountedRelease(countedReleases)
	}
	if len(errList) > 0 {
		return fmt.Errorf(strings.Join(errList, ","))
	}
	return nil
}

func runCountReleasesCommand(cmd *cobra.Command, args []string) error {
	counter := 0

	configurator, err := configurator.NewClusterConfiguratorFromKubeConfig(kubeConfigFile, managementClusterContext)
	if err != nil {
		return err
	}

	namespaceList, err := configurator.KubeClient.CoreV1().Namespaces().List(metav1.ListOptions{})
	if err != nil {
		return err
	}
	var errList []string
	var countedReleases []OutputRelease
	for _, ns := range namespaceList.Items {
		releaseList, err := configurator.ShipperClient.ShipperV1alpha1().Releases(ns.Name).List(metav1.ListOptions{})
		if err != nil {
			errList = append(errList, err.Error())
			continue
		}
		for _, rel := range releaseList.Items {
			trueClusters := getFilteredSelectedClusters(&rel)
			if len(trueClusters) == 0 {
				counter++
				countedReleases = append(
					countedReleases,
					OutputRelease{
						Namespace: rel.Namespace,
						Name:      rel.Name,
					})
			}
		}
	}

	if printOption == "" {
		cmd.Println("Number of *releases* that are scheduled only on decommissioned clusters: ", counter)
	} else {
		printCountedRelease(countedReleases)
	}
	if len(errList) > 0 {
		return fmt.Errorf(strings.Join(errList, ","))
	}
	return nil
}

func printCountedRelease(outputReleases []OutputRelease) {
	var err error
	var data []byte

	switch printOption {
	case "yaml":
		data, err = yaml.Marshal(outputReleases)
	case "json":
		data, err = json.MarshalIndent(outputReleases, "", "    ")
	case "":
		return
	}
	if err != nil {
		os.Stderr.Write(bytes.NewBufferString(err.Error()).Bytes())
		return
	}

	_, _ = os.Stdout.Write(data)
}

func getFilteredSelectedClusters(rel *shipper.Release) []string {
	clusters := releaseutil.GetSelectedClusters(rel)
	var trueClusters []string
	for _, cluster := range clusters {
		if !filters.SliceContainsString(decommissionedClusters, cluster) {
			trueClusters = append(trueClusters, cluster)
		}
	}
	return trueClusters
}

func isContender(rel *shipper.Release, configurator *configurator.Cluster) (bool, error) {
	appName := rel.Labels[shipper.AppLabel]
	app, err := configurator.ShipperClient.ShipperV1alpha1().Applications(rel.Namespace).Get(appName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	contender, err := getContender(app, configurator)
	if err != nil {
		return false, err
	}
	return contender.Name == rel.Name && contender.Namespace == rel.Namespace, nil
}

func getContender(app *shipper.Application, configurator *configurator.Cluster) (*shipper.Release, error) {
	appName := app.Name
	selector := labels.Set{shipper.AppLabel: appName}.AsSelector()
	releaseList, err := configurator.ShipperClient.ShipperV1alpha1().Releases(app.Namespace).List(metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, err
	}
	rels := make([]*shipper.Release, len(releaseList.Items))
	for i, _ := range releaseList.Items {
		rels[i] = &releaseList.Items[i]
	}
	rels = releaseutil.SortByGenerationDescending(rels)
	contender, err := apputil.GetContender(appName, rels)
	if err != nil {
		return nil, err
	}
	return contender, nil
}
