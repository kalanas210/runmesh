import type { Metadata } from "next";
import { Composer } from "@/components/submit/composer";

export const metadata: Metadata = { title: "Submit" };

export default function SubmitPage() {
  return <Composer />;
}
