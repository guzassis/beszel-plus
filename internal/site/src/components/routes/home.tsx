import { useLingui } from "@lingui/react/macro"
import { memo, Suspense, useEffect, useMemo } from "react"
import SystemsTable from "@/components/systems-table/systems-table"
import { ActiveAlerts } from "@/components/active-alerts"
import { FooterRepoLink } from "@/components/footer-repo-link"
import { pageTitle } from "@/lib/build-info"

export default memo(() => {
	const { t } = useLingui()

	useEffect(() => {
		document.title = pageTitle(t`All Systems`)
	}, [t])

	return useMemo(
		() => (
			<>
				<div className="flex flex-col gap-4">
					<ActiveAlerts />
					<Suspense>
						<SystemsTable />
					</Suspense>
				</div>
				<FooterRepoLink />
			</>
		),
		[]
	)
})
