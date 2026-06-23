/**
* This file was @generated using pocketbase-typegen
*/

import type PocketBase from 'pocketbase'
import type { RecordService } from 'pocketbase'

export const Collections = {
	Apikeys: "_apiKeys",
	Authorigins: "_authOrigins",
	Externalauths: "_externalAuths",
	Mfas: "_mfas",
	Otps: "_otps",
	Permissions: "_permissions",
	Roles: "_roles",
	Serviceaccounts: "_serviceAccounts",
	Superusers: "_superusers",
	Articles: "articles",
	Clients: "clients",
	Invoices: "invoices",
	McpDemo: "mcp_demo",
	Projects: "projects",
	Tasks: "tasks",
	Users: "users",
} as const
export type Collections = typeof Collections[keyof typeof Collections]

// Alias types for improved usability
export type IsoDateString = string
export type IsoAutoDateString = string & { readonly autodate: unique symbol }
export type RecordIdString = string
export type FileNameString = string & { readonly filename: unique symbol }
export type HTMLString = string

type ExpandType<T> = unknown extends T
	? T extends unknown
		? { expand?: unknown }
		: { expand: T }
	: { expand: T }

// System fields
export type BaseSystemFields<T = unknown> = {
	id: RecordIdString
	collectionId: string
	collectionName: Collections
} & ExpandType<T>

export type AuthSystemFields<T = unknown> = {
	email: string
	emailVisibility: boolean
	username: string
	verified: boolean
} & BaseSystemFields<T>

// Record types for each collection

export type ApikeysRecord = {
	created: IsoAutoDateString
	expiresUnix?: number
	hash: string
	id: string
	lastUsedUnix?: number
	name: string
	prefix?: string
	revoked?: boolean
	scopes?: string
	serviceAccountId?: string
	superuserId?: string
}

export type AuthoriginsRecord = {
	collectionRef: string
	created: IsoAutoDateString
	fingerprint: string
	id: string
	recordRef: string
	updated: IsoAutoDateString
}

export type ExternalauthsRecord = {
	collectionRef: string
	created: IsoAutoDateString
	id: string
	provider: string
	providerId: string
	recordRef: string
	updated: IsoAutoDateString
}

export type MfasRecord = {
	collectionRef: string
	created: IsoAutoDateString
	id: string
	method: string
	recordRef: string
	updated: IsoAutoDateString
}

export type OtpsRecord = {
	collectionRef: string
	created: IsoAutoDateString
	id: string
	password: string
	recordRef: string
	sentTo?: string
	updated: IsoAutoDateString
}

export type PermissionsRecord = {
	created: IsoAutoDateString
	description?: string
	id: string
	token: string
}

export type RolesRecord = {
	created: IsoAutoDateString
	description?: string
	id: string
	name: string
	permissions?: RecordIdString[]
}

export type ServiceaccountsRecord = {
	email: string
	emailVisibility?: boolean
	id: string
	label?: string
	password: string
	roles?: RecordIdString[]
	tokenKey: string
	verified?: boolean
}

export type SuperusersRecord = {
	created: IsoAutoDateString
	email: string
	emailVisibility?: boolean
	id: string
	password: string
	tokenKey: string
	updated: IsoAutoDateString
	verified?: boolean
}

export type ArticlesRecord = {
	body?: string
	id: string
	title: string
}

export type ClientsRecord = {
	created: IsoAutoDateString
	email?: string
	id: string
	name: string
}

export const InvoicesStatusOptions = {
	"draft": "draft",
	"paid": "paid",
	"void": "void",
} as const
export type InvoicesStatusOptions = typeof InvoicesStatusOptions[keyof typeof InvoicesStatusOptions]
export type InvoicesRecord = {
	amount: number
	client: RecordIdString
	created: IsoAutoDateString
	id: string
	status: InvoicesStatusOptions
}

export type McpDemoRecord = {
	created: IsoAutoDateString
	id: string
	title: string
}

export type ProjectsRecord = {
	active?: boolean
	archived?: boolean
	budget?: number
	created: IsoAutoDateString
	id: string
	owner?: string
	priority?: number
	title: string
}

export const TasksStatusOptions = {
	"todo": "todo",
	"doing": "doing",
	"done": "done",
} as const
export type TasksStatusOptions = typeof TasksStatusOptions[keyof typeof TasksStatusOptions]

export const TasksLabelsOptions = {
	"bug": "bug",
	"feature": "feature",
	"chore": "chore",
} as const
export type TasksLabelsOptions = typeof TasksLabelsOptions[keyof typeof TasksLabelsOptions]
export type TasksRecord = {
	created: IsoAutoDateString
	done?: boolean
	id: string
	labels?: TasksLabelsOptions[]
	name: string
	project: RecordIdString
	status: TasksStatusOptions
}

export type UsersRecord = {
	avatar?: FileNameString
	created: IsoAutoDateString
	email: string
	emailVisibility?: boolean
	id: string
	name?: string
	password: string
	roles?: RecordIdString[]
	tokenKey: string
	updated: IsoAutoDateString
	verified?: boolean
}

// Response types include system fields and match responses from the PocketBase API
export type ApikeysResponse<Texpand = unknown> = Required<ApikeysRecord> & BaseSystemFields<Texpand>
export type AuthoriginsResponse<Texpand = unknown> = Required<AuthoriginsRecord> & BaseSystemFields<Texpand>
export type ExternalauthsResponse<Texpand = unknown> = Required<ExternalauthsRecord> & BaseSystemFields<Texpand>
export type MfasResponse<Texpand = unknown> = Required<MfasRecord> & BaseSystemFields<Texpand>
export type OtpsResponse<Texpand = unknown> = Required<OtpsRecord> & BaseSystemFields<Texpand>
export type PermissionsResponse<Texpand = unknown> = Required<PermissionsRecord> & BaseSystemFields<Texpand>
export type RolesResponse<Texpand = unknown> = Required<RolesRecord> & BaseSystemFields<Texpand>
export type ServiceaccountsResponse<Texpand = unknown> = Required<ServiceaccountsRecord> & AuthSystemFields<Texpand>
export type SuperusersResponse<Texpand = unknown> = Required<SuperusersRecord> & AuthSystemFields<Texpand>
export type ArticlesResponse<Texpand = unknown> = Required<ArticlesRecord> & BaseSystemFields<Texpand>
export type ClientsResponse<Texpand = unknown> = Required<ClientsRecord> & BaseSystemFields<Texpand>
export type InvoicesResponse<Texpand = unknown> = Required<InvoicesRecord> & BaseSystemFields<Texpand>
export type McpDemoResponse<Texpand = unknown> = Required<McpDemoRecord> & BaseSystemFields<Texpand>
export type ProjectsResponse<Texpand = unknown> = Required<ProjectsRecord> & BaseSystemFields<Texpand>
export type TasksResponse<Texpand = unknown> = Required<TasksRecord> & BaseSystemFields<Texpand>
export type UsersResponse<Texpand = unknown> = Required<UsersRecord> & AuthSystemFields<Texpand>

// Types containing all Records and Responses, useful for creating typing helper functions

export type CollectionRecords = {
	_apiKeys: ApikeysRecord
	_authOrigins: AuthoriginsRecord
	_externalAuths: ExternalauthsRecord
	_mfas: MfasRecord
	_otps: OtpsRecord
	_permissions: PermissionsRecord
	_roles: RolesRecord
	_serviceAccounts: ServiceaccountsRecord
	_superusers: SuperusersRecord
	articles: ArticlesRecord
	clients: ClientsRecord
	invoices: InvoicesRecord
	mcp_demo: McpDemoRecord
	projects: ProjectsRecord
	tasks: TasksRecord
	users: UsersRecord
}

export type CollectionResponses = {
	_apiKeys: ApikeysResponse
	_authOrigins: AuthoriginsResponse
	_externalAuths: ExternalauthsResponse
	_mfas: MfasResponse
	_otps: OtpsResponse
	_permissions: PermissionsResponse
	_roles: RolesResponse
	_serviceAccounts: ServiceaccountsResponse
	_superusers: SuperusersResponse
	articles: ArticlesResponse
	clients: ClientsResponse
	invoices: InvoicesResponse
	mcp_demo: McpDemoResponse
	projects: ProjectsResponse
	tasks: TasksResponse
	users: UsersResponse
}

// Utility types for create/update operations

type ProcessCreateAndUpdateFields<T> = Omit<{
	// Omit AutoDate fields
	[K in keyof T as Extract<T[K], IsoAutoDateString> extends never ? K : never]: 
		// Convert FileNameString to File
		T[K] extends infer U ? 
			U extends (FileNameString | FileNameString[]) ? 
				U extends any[] ? File[] : File 
			: U
		: never
}, 'id'>

// Create type for Auth collections
export type CreateAuth<T> = {
	id?: RecordIdString
	email: string
	emailVisibility?: boolean
	password: string
	passwordConfirm: string
	verified?: boolean
} & ProcessCreateAndUpdateFields<T>

// Create type for Base collections
export type CreateBase<T> = {
	id?: RecordIdString
} & ProcessCreateAndUpdateFields<T>

// Update type for Auth collections
export type UpdateAuth<T> = Partial<
	Omit<ProcessCreateAndUpdateFields<T>, keyof AuthSystemFields>
> & {
	email?: string
	emailVisibility?: boolean
	oldPassword?: string
	password?: string
	passwordConfirm?: string
	verified?: boolean
}

// Update type for Base collections
export type UpdateBase<T> = Partial<
	Omit<ProcessCreateAndUpdateFields<T>, keyof BaseSystemFields>
>

// Get the correct create type for any collection
export type Create<T extends keyof CollectionResponses> =
	CollectionResponses[T] extends AuthSystemFields
		? CreateAuth<CollectionRecords[T]>
		: CreateBase<CollectionRecords[T]>

// Get the correct update type for any collection
export type Update<T extends keyof CollectionResponses> =
	CollectionResponses[T] extends AuthSystemFields
		? UpdateAuth<CollectionRecords[T]>
		: UpdateBase<CollectionRecords[T]>

// Type for usage with type asserted PocketBase instance
// https://github.com/pocketbase/js-sdk#specify-typescript-definitions

export type TypedPocketBase = {
	collection<T extends keyof CollectionResponses>(
		idOrName: T
	): RecordService<CollectionResponses[T]>
} & PocketBase
