package main

import (
	"encoding/json"

	"github.com/spf13/cobra"

	"knowledge-ba46-flow/internal/review"
)

func reconcileCmd() *cobra.Command {
	var token string
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Preview queue validity; explicitly confirm retirement of obsolete proposals",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, db, err := openAll(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()
			var plan review.Reconciliation
			if token == "" {
				plan, err = review.PreviewReconciliation(cmd.Context(), c, db)
			} else {
				plan, err = review.ConfirmReconciliation(cmd.Context(), c, db, token)
			}
			if err != nil {
				return err
			}
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			return encoder.Encode(plan)
		},
	}
	cmd.Flags().StringVar(&token, "confirm", "", "Retire obsolete proposals only if this preview token is still current")
	return cmd
}

func identifyCmd() *cobra.Command {
	var prepare bool
	var note string
	cmd := &cobra.Command{
		Use:   "identify",
		Short: "Preview immutable document IDs; optionally prepare reviewed initialization proposals",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, db, err := openAll(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()
			plan, err := review.PlanIdentities(cmd.Context(), c, db, note, prepare)
			if err != nil {
				return err
			}
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			return encoder.Encode(plan)
		},
	}
	cmd.Flags().BoolVar(&prepare, "prepare", false, "Create pending UUID initialization proposals; never write notes automatically")
	cmd.Flags().StringVar(&note, "note", "", "Limit identity initialization to one scanned note (repository-relative path)")
	return cmd
}
