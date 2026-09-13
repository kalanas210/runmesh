import type { Metadata } from "next";
import { ToolCatalogue } from "@/components/tools/tool-catalogue";

export const metadata: Metadata = { title: "Tools" };

export default function ToolsPage() {
  return <ToolCatalogue />;
}
