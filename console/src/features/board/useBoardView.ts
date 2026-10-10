import { useState } from "react";

import { type BoardView, getStoredBoardView, setStoredBoardView } from "@/platform/boardPrefs";

/**
 * Board or List, remembered per browser. The board is the default: it is
 * what Mission Control is, and the list is the same requests one click away.
 */
export function useBoardView(): readonly [BoardView, (view: BoardView) => void] {
  const [view, setView] = useState<BoardView>(() => getStoredBoardView() ?? "board");
  return [
    view,
    (next) => {
      setStoredBoardView(next);
      setView(next);
    },
  ];
}
