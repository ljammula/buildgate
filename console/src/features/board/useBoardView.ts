import { useState } from "react";

import { type BoardView, getStoredBoardView, setStoredBoardView } from "@/platform/boardPrefs";

export interface BoardViewControls {
  readonly view: BoardView;
  /** The operator's choice: shown now and remembered by the browser. */
  readonly choose: (view: BoardView) => void;
  /**
   * Shows a view for this visit only, leaving the remembered choice alone:
   * for a link that happens to lead to the other view ("Show all" on Done),
   * which is not the operator changing their preference.
   */
  readonly showOnce: (view: BoardView) => void;
}

/**
 * Board or List, remembered per browser. The board is the default: it is
 * what Mission Control is, and the list is the same requests one click away.
 */
export function useBoardView(): BoardViewControls {
  const [view, setView] = useState<BoardView>(() => getStoredBoardView() ?? "board");
  return {
    view,
    choose: (next) => {
      setStoredBoardView(next);
      setView(next);
    },
    showOnce: setView,
  };
}
