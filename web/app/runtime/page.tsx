import type { Metadata } from "next";
import { RuntimePanel } from "@/components/runtime/runtime-panel";

export const metadata: Metadata = { title: "Runtime" };

export default function RuntimePage() {
  return <RuntimePanel />;
}
