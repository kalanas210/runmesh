import type { Metadata } from "next";
import { Gallery } from "@/components/design/gallery";

export const metadata: Metadata = { title: "Primitives" };

export default function DesignPage() {
  return <Gallery />;
}
