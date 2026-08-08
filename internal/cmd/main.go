package cmd

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/syndbg/fabric-x-migrate-poc/internal/fabricsnapshot"
	"github.com/syndbg/fabric-x-migrate-poc/pkg/genesisdata"
)

const exporterVersion = "0.1.0-poc"

// Run executes the migration CLI.
func Run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: fabric-x-migrate <export|verify>")
	}
	switch args[0] {
	case "export":
		return export(args[1:], stdout, stderr)
	case "verify":
		return verify(args[1:], stdout, stderr)
	case "verify-source":
		return verifySource(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func verifySource(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("verify-source", flag.ContinueOnError)
	flags.SetOutput(stderr)
	snapshotPath := flags.String("snapshot", "", "Fabric peer snapshot directory")
	inputPath := flags.String("input", "", "input .fxgenesis file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *snapshotPath == "" || *inputPath == "" {
		return errors.New("--snapshot and --input are required")
	}
	snapshot, err := fabricsnapshot.Read(*snapshotPath)
	if err != nil {
		return err
	}
	file, err := genesisdata.Read(*inputPath)
	if err != nil {
		return err
	}
	expected, err := exportInput(snapshot, file.Manifest.Source.FabricVersion, file.Manifest.NamespaceMappings)
	if err != nil {
		return err
	}
	expected.ExporterVersion = file.Manifest.ExporterVersion
	if err := genesisdata.VerifyInput(file, expected); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "migration_id: %s\nsource: channel %s block %d\npublic_records: %d\ntransaction_ids: %d\nsource_integrity: verified\n",
		file.MigrationID, file.Manifest.Source.Channel, file.Manifest.Source.LastBlockNumber,
		len(file.PublicState), len(file.TransactionIDs))
	return err
}

func export(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("export", flag.ContinueOnError)
	flags.SetOutput(stderr)
	snapshotPath := flags.String("snapshot", "", "Fabric peer snapshot directory")
	output := flags.String("output", "", "output .fxgenesis file")
	var mappings namespaceMappings
	flags.Var(&mappings, "namespace", "repeatable namespace mapping SOURCE=TARGET")
	fabricVersion := flags.String("fabric-version", "", "source Fabric version")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *snapshotPath == "" || *output == "" || len(mappings) == 0 || *fabricVersion == "" {
		return errors.New("--snapshot, --output, --namespace, and --fabric-version are required")
	}

	snapshot, err := fabricsnapshot.Read(*snapshotPath)
	if err != nil {
		return err
	}
	input, err := exportInput(snapshot, *fabricVersion, mappings)
	if err != nil {
		return err
	}
	file, err := genesisdata.Write(*output, input)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "wrote %s\nmigration_id: %s\npublic_records: %d\ntransaction_ids: %d\n", *output, file.MigrationID, len(file.PublicState), len(file.TransactionIDs))
	return err
}

type namespaceMappings []genesisdata.NamespaceMapping

func (m *namespaceMappings) String() string {
	values := make([]string, len(*m))
	for i, mapping := range *m {
		values[i] = mapping.Source + "=" + mapping.Target
	}
	return strings.Join(values, ",")
}

func (m *namespaceMappings) Set(value string) error {
	source, target, ok := strings.Cut(value, "=")
	if !ok || source == "" || target == "" {
		return errors.New("--namespace must be SOURCE=TARGET")
	}
	*m = append(*m, genesisdata.NamespaceMapping{Source: source, Target: target})
	return nil
}

func exportInput(snapshot *fabricsnapshot.Snapshot, fabricVersion string, mappings []genesisdata.NamespaceMapping) (genesisdata.Input, error) {
	input := genesisdata.Input{
		ExporterVersion: exporterVersion,
		Source: genesisdata.Source{
			FabricVersion: fabricVersion, Channel: snapshot.Metadata.ChannelName,
			LastBlockNumber: snapshot.Metadata.LastBlockNumber, LastBlockHash: snapshot.Metadata.LastBlockHash,
			PreviousBlockHash:   snapshot.Metadata.PreviousBlockHash,
			LastBlockCommitHash: snapshot.AdditionalMetadata.LastBlockCommitHash,
			SnapshotHash:        snapshot.AdditionalMetadata.SnapshotHash, StateDBType: snapshot.Metadata.StateDBType,
		},
		NamespaceMappings: append([]genesisdata.NamespaceMapping(nil), mappings...),
	}
	targetBySource := make(map[string]string, len(mappings))
	for _, mapping := range mappings {
		targetBySource[mapping.Source] = mapping.Target
	}
	for _, file := range snapshot.Files {
		input.Source.Files = append(input.Source.Files, genesisdata.SourceFile{
			Name: file.Name, Size: file.Size, SHA256: file.SHA256, FormatByte: file.FormatByte,
		})
	}
	found := make(map[string]bool, len(mappings))
	for _, record := range snapshot.PublicRecords {
		targetNamespace, selected := targetBySource[record.Namespace]
		if !selected {
			continue
		}
		found[record.Namespace] = true
		if len(record.Metadata) != 0 {
			return genesisdata.Input{}, fmt.Errorf("namespace %q contains key metadata that this PoC cannot preserve", record.Namespace)
		}
		input.PublicState = append(input.PublicState, &genesisdata.StateRecord{
			SourceNamespace: record.Namespace, TargetNamespace: targetNamespace,
			Key: record.Key, Value: record.Value, Metadata: record.Metadata, SourceVersion: record.Version,
			SourceBlockNumber: record.BlockNumber, SourceTransactionNumber: record.TransactionNumber,
		})
	}
	for _, mapping := range mappings {
		if !found[mapping.Source] {
			return genesisdata.Input{}, fmt.Errorf("source namespace %q has no public records", mapping.Source)
		}
	}
	for _, namespace := range snapshot.PublicNamespaces {
		if _, selected := targetBySource[namespace.Name]; selected {
			continue
		}
		subject := namespace.Name
		if subject == "" {
			subject = "<empty>"
		}
		input.Exclusions = append(input.Exclusions, genesisdata.Exclusion{
			Kind: "public_namespace", Subject: subject, RecordCount: namespace.Records,
			SourceFiles: []string{"public_state.data", "public_state.metadata"},
			Reason:      "namespace is not selected by --namespace",
		})
	}
	for _, namespace := range snapshot.PrivateHashNamespaces {
		input.Exclusions = append(input.Exclusions, genesisdata.Exclusion{
			Kind: "private_data_hashes", Subject: namespace.Name, RecordCount: namespace.Records,
			SourceFiles: []string{"private_state_hashes.data", "private_state_hashes.metadata"},
			Reason:      "Fabric-X has no equivalent private-data collection state",
		})
	}
	if snapshot.ConfigHistoryRecords > 0 {
		input.Exclusions = append(input.Exclusions, genesisdata.Exclusion{
			Kind: "collection_config_history", Subject: "all", RecordCount: snapshot.ConfigHistoryRecords,
			SourceFiles: []string{"confighistory.data", "confighistory.metadata"},
			Reason:      "Fabric-X has no equivalent collection configuration history",
		})
	}
	for _, id := range snapshot.TransactionIDs {
		input.TransactionIDs = append(input.TransactionIDs, &genesisdata.TransactionIDRecord{TransactionId: id})
	}
	return input, nil
}

func verify(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	input := flags.String("input", "", "input .fxgenesis file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *input == "" {
		return errors.New("--input is required")
	}
	file, err := genesisdata.Read(*input)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "valid: %s\nmigration_id: %s\nsource: Fabric %s channel %s block %d\npublic_records: %d\ntransaction_ids: %d\n", *input, file.MigrationID, file.Manifest.Source.FabricVersion, file.Manifest.Source.Channel, file.Manifest.Source.LastBlockNumber, len(file.PublicState), len(file.TransactionIDs))
	return err
}
