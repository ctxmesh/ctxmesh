// The primitive kit — the scale-first component vocabulary every console-arc
// surface (M13–M18) composes (spec §5). Skeletons here in m13.1 (design gate);
// they become the vitest-covered production kit in m13.4. Import from
// "@/components/kit" so surfaces never reach into individual files.

export { DataTable, cellNum, CellEntity, CellId, truncateId } from "./data-table";
export type {
  Column,
  ColumnPriority,
  DataTableProps,
  DataTableError,
  SortState,
  SortDir,
} from "./data-table";

export { Wizard } from "./wizard";
export type { WizardStep, WizardProps } from "./wizard";

export { DetailDrawer } from "./detail-drawer";
export type { DetailDrawerProps } from "./detail-drawer";

export { ConfirmDialog } from "./confirm-dialog";
export type { ConfirmDialogProps } from "./confirm-dialog";

export { EmptyState } from "./empty-state";
export type { EmptyStateProps, EmptyStateAction } from "./empty-state";

export { ErrorState } from "./error-state";
export type { ErrorStateProps } from "./error-state";

export { ForbiddenInline } from "./forbidden-inline";
export type { ForbiddenInlineProps } from "./forbidden-inline";

export {
  StatusBadge,
  resolveStatus,
  humanizeStatusReason,
  type StatusTone,
  type StatusVariant,
} from "./status-badge";
export { VisibilityBadge } from "./visibility-badge";

export { ComboSelect } from "./combo-select";

export { ResourceLink, resourcePath } from "./resource-link";
export type { ResourceKind } from "./resource-link";

export {
  Skeleton,
  SkeletonText,
  SkeletonTable,
  SkeletonCard,
} from "./skeleton";
export type { SkeletonProps } from "./skeleton";

export { CommandPalette, useCommandK } from "./command-palette";
export type { CommandItem, CommandPaletteProps } from "./command-palette";

export {
  Toast,
  ToastProvider,
  useToast,
  DEFAULT_TOAST_DURATION,
} from "./toast";
export type {
  ToastProps,
  ToastVariant,
  ToastOptions,
  ToastContextValue,
} from "./toast";

export { useFocusTrap } from "./use-focus-trap";
export type { FocusTrapOptions } from "./use-focus-trap";

export { CredentialSourceBadge } from "./credential-source-badge";

// ── The editorial kit (M151, spec §5.17–5.28) ───────────────────────────────
// Twelve components the redesign needed and the console did not have. They are
// exported here rather than reached into directly so a page never imports two
// different ways of saying the same thing — which is exactly what happened
// before `quantity.tsx` existed: three components had each invented their own
// "we do not know this" glyph.

// The one place the console decides what a number MEANS. `Quantity` is not
// assignable to `number`, so an unknown cannot be arithmetic'd or rendered as a
// figure — the honest branch is enforced by the compiler, not by discipline.
export {
  UNKNOWN,
  UNKNOWN_GLYPH,
  UNKNOWN_TITLE,
  ZERO_CLASS,
  isKnown,
  formatCount,
  speakQuantity,
  QuantityValue,
  UnknownValue,
  MeasureNote,
  type Quantity,
  type Unknown,
} from "./quantity";

export { PageHeader } from "./page-header";
export type {
  PageHeaderProps,
  PageHeaderCrumb,
  PageHeaderAction,
  PageHeaderTab,
} from "./page-header";

export { SectionHeader, ClosingNote } from "./section-header";
export type { SectionHeaderProps, ClosingNoteProps } from "./section-header";

export {
  NextStepLink,
  nextStepRank,
  NEXT_STEP_MAX_CHARS,
  NOTHING_NEEDED,
} from "./next-step-link";
export type { NextStepLinkProps, NextStepTone } from "./next-step-link";

export { LifecycleStrip, LifecycleTrack, LIFECYCLE_STAGES, lifecycleFactNumber } from "./lifecycle";
export type {
  LifecycleStage,
  LifecycleStageCell,
  LifecycleStripProps,
  LifecycleTrackProps,
} from "./lifecycle";

export { PressureStrip } from "./pressure-strip";
export type { PressureStripProps } from "./pressure-strip";

export { Meter, meterState } from "./meter";
export type { MeterProps, MeterState } from "./meter";

export {
  TreeTable,
  CellCount,
  TREE_ROW_HEIGHT,
  TREE_SUBROW_HEIGHT,
  TREE_INDENT_PX,
  TREE_GUTTER_MAX_LEVELS,
} from "./tree-table";
export type {
  TreeRow,
  TreeRowKind,
  TreeColumn,
  TreeNameTone,
  TreeTableProps,
} from "./tree-table";

export {
  StopControl,
  StopNotice,
  STOP_CONTRACT,
  STOP_HELD_EXPLAINER,
  STOP_LIMIT,
  STOP_REASON_LABEL,
  STOP_REASON_HINT,
  STOP_REASON_REQUIRED,
  STOP_REASON_MAX,
  STOP_NOTICE_CONTRACT,
  LIFT_CONTRACT,
} from "./stop";
export type {
  StopScopeKind,
  StopImpact,
  StopScopeOption,
  StopRequest,
  StopControlProps,
  StopNoticeProps,
} from "./stop";

export { KeyValueList, KV_ABSENT_DEFAULT } from "./kv-list";
export type { KeyValueItem, KeyValueListProps } from "./kv-list";

export { Timeline, TimelineSkeleton } from "./timeline";
export type { TimelineProps, TimelineStep, TimelineTone } from "./timeline";

export { QuietNote, UNKNOWN_VALUE_TITLE } from "./quiet-note";
export type { QuietNoteProps } from "./quiet-note";

export { FilterChipRow } from "./filter-chips";
export type { FilterChip, FilterChipRowProps } from "./filter-chips";

// Applied by BOTH tables, so the column budget is one design decision.
export { PRIORITY_CLASS, columnPriority } from "./data-table";
