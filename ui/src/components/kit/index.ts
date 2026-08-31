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
