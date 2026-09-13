"use client";

import { useQuery } from "@tanstack/react-query";
import { apiJson } from "@/lib/api";
import type { ToolsResponse } from "@/lib/types";

/**
 * GET /api/v1/tools.
 *
 * The catalogue is deployment configuration, not runtime state: it changes
 * when a process is restarted with a different policy, and never between two
 * reads of the same process. So this does not poll at all, and the staleTime
 * is a minute rather than the zero the library defaults to.
 *
 * That is not a micro-optimisation. `refetchOnWindowFocus` is on globally, and
 * with a zero staleTime every alt-tab back to a console left open on /tools
 * would re-fetch a list that cannot have changed. A minute is short enough that
 * a reader who has just redeployed and switched back sees the new policy, and
 * long enough that reading the page is not a request.
 */
export function useTools() {
  return useQuery({
    queryKey: ["tools"],
    queryFn: () => apiJson<ToolsResponse>("/tools"),
    staleTime: 60_000,
  });
}
